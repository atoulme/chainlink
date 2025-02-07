package log

import (
	"database/sql"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/pkg/errors"

	"github.com/smartcontractkit/chainlink/core/services/postgres"
	"github.com/smartcontractkit/chainlink/core/utils"
	"github.com/smartcontractkit/sqlx"
)

//go:generate mockery --name ORM --output ./mocks/ --case=underscore --structname ORM --filename orm.go

// ORM is the interface for log broadcasts.
//  - Unconsumed broadcasts are created just before notifying subscribers, who are responsible for marking them consumed.
//  - Pending broadcast block numbers are synced to the min from the pool (or deleted when empty)
//  - On reboot, backfill considers the min block number from unconsumed and pending broadcasts. Additionally, unconsumed
//    entries are removed and the pending broadcasts number updated.
//
type ORM interface {
	// FindBroadcasts returns broadcasts for a range of block numbers, both consumed and unconsumed.
	FindBroadcasts(fromBlockNum int64, toBlockNum int64) ([]LogBroadcast, error)
	// CreateBroadcast inserts an unconsumed log broadcast for jobID.
	CreateBroadcast(blockHash common.Hash, blockNumber uint64, logIndex uint, jobID int32, qopts ...postgres.QOpt) error
	// WasBroadcastConsumed returns true if jobID consumed the log broadcast.
	WasBroadcastConsumed(blockHash common.Hash, logIndex uint, jobID int32, qopts ...postgres.QOpt) (bool, error)
	// MarkBroadcastConsumed marks the log broadcast as consumed by jobID.
	MarkBroadcastConsumed(blockHash common.Hash, blockNumber uint64, logIndex uint, jobID int32, qopts ...postgres.QOpt) error

	// SetPendingMinBlock sets the minimum block number for which there are pending broadcasts in the pool, or nil if empty.
	SetPendingMinBlock(blockNum *int64, qopts ...postgres.QOpt) error
	// GetPendingMinBlock returns the minimum block number for which there were pending broadcasts in the pool, or nil if it was empty.
	GetPendingMinBlock(qopts ...postgres.QOpt) (blockNumber *int64, err error)

	// Reinitialize cleans up the database by removing any unconsumed broadcasts, then updating (if necessary) and
	// returning the pending minimum block number.
	Reinitialize(qopts ...postgres.QOpt) (blockNumber *int64, err error)
}

type orm struct {
	db         *sqlx.DB
	evmChainID utils.Big
}

var _ ORM = (*orm)(nil)

func NewORM(db *sqlx.DB, evmChainID big.Int) *orm {
	return &orm{db, *utils.NewBig(&evmChainID)}
}

func (o *orm) WasBroadcastConsumed(blockHash common.Hash, logIndex uint, jobID int32, qopts ...postgres.QOpt) (consumed bool, err error) {
	query := `
		SELECT consumed FROM log_broadcasts
		WHERE block_hash = $1
		AND log_index = $2
		AND job_id = $3
		AND evm_chain_id = $4
    `
	args := []interface{}{
		blockHash,
		logIndex,
		jobID,
		o.evmChainID,
	}
	q := postgres.NewQ(o.db, qopts...)
	err = q.QueryRowx(query, args...).Scan(&consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return consumed, err
}

func (o *orm) FindBroadcasts(fromBlockNum int64, toBlockNum int64) ([]LogBroadcast, error) {
	var broadcasts []LogBroadcast
	query := `
		SELECT block_hash, consumed, log_index, job_id FROM log_broadcasts
		WHERE block_number >= $1
		AND block_number <= $2
		AND evm_chain_id = $3
	`
	err := o.db.Select(&broadcasts, query, fromBlockNum, toBlockNum, o.evmChainID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to find log broadcasts")
	}
	return broadcasts, err
}

func (o *orm) CreateBroadcast(blockHash common.Hash, blockNumber uint64, logIndex uint, jobID int32, qopts ...postgres.QOpt) error {
	q := postgres.NewQ(o.db, qopts...)
	_, err := q.Exec(`
        INSERT INTO log_broadcasts (block_hash, block_number, log_index, job_id, created_at, updated_at, consumed, evm_chain_id)
		VALUES ($1, $2, $3, $4, NOW(), NOW(), false, $5)
    `, blockHash, blockNumber, logIndex, jobID, o.evmChainID)
	return errors.Wrap(err, "failed to create log broadcast")
}

func (o *orm) MarkBroadcastConsumed(blockHash common.Hash, blockNumber uint64, logIndex uint, jobID int32, qopts ...postgres.QOpt) error {
	q := postgres.NewQ(o.db, qopts...)
	_, err := q.Exec(`
        INSERT INTO log_broadcasts (block_hash, block_number, log_index, job_id, created_at, updated_at, consumed, evm_chain_id)
		VALUES ($1, $2, $3, $4, NOW(), NOW(), true, $5)
		ON CONFLICT (job_id, block_hash, log_index, evm_chain_id) DO UPDATE
		SET consumed = true, updated_at = NOW()
    `, blockHash, blockNumber, logIndex, jobID, o.evmChainID)
	return errors.Wrap(err, "failed to mark log broadcast as consumed")
}

func (o *orm) Reinitialize(qopts ...postgres.QOpt) (*int64, error) {
	// Minimum block number from the set of unconsumed logs, which we'll remove later.
	minUnconsumed, err := o.getUnconsumedMinBlock(qopts...)
	if err != nil {
		return nil, err
	}
	// Minimum block number from the set of pending logs in the pool.
	minPending, err := o.GetPendingMinBlock(qopts...)
	if err != nil {
		return nil, err
	}
	if minUnconsumed == nil {
		// Nothing unconsumed to consider or cleanup, and pending minimum block number still stands.
		return minPending, nil
	}
	if minPending == nil || *minUnconsumed < *minPending {
		// Use the lesser minUnconsumed.
		minPending = minUnconsumed
		// Update the db so that we can safely delete the unconsumed entries.
		if err := o.SetPendingMinBlock(minPending, qopts...); err != nil {
			return nil, err
		}
	}
	// Safe to delete old unconsumed entries since the pending minimum block covers this range.
	if err := o.removeUnconsumed(qopts...); err != nil {
		return nil, err
	}
	return minPending, nil
}

func (o *orm) SetPendingMinBlock(blockNumber *int64, qopts ...postgres.QOpt) error {
	q := postgres.NewQ(o.db, qopts...)
	_, err := q.Exec(`
        INSERT INTO log_broadcasts_pending (evm_chain_id, block_number, created_at, updated_at) VALUES ($1, $2, NOW(), NOW())
		ON CONFLICT (evm_chain_id) DO UPDATE SET block_number = $3, updated_at = NOW() 
    `, o.evmChainID, blockNumber, blockNumber)
	return errors.Wrap(err, "failed to set pending broadcast block number")
}

func (o *orm) GetPendingMinBlock(qopts ...postgres.QOpt) (*int64, error) {
	q := postgres.NewQ(o.db, qopts...)
	var blockNumber *int64
	err := q.Get(&blockNumber, `
        SELECT block_number FROM log_broadcasts_pending WHERE evm_chain_id = $1
    `, o.evmChainID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, errors.Wrap(err, "failed to get broadcasts pending number")
	}
	return blockNumber, nil
}

func (o *orm) getUnconsumedMinBlock(qopts ...postgres.QOpt) (*int64, error) {
	q := postgres.NewQ(o.db, qopts...)
	var blockNumber *int64
	err := q.Get(&blockNumber, `
        SELECT min(block_number) FROM log_broadcasts
			WHERE evm_chain_id = $1
			AND consumed = false
			AND block_number IS NOT NULL
    `, o.evmChainID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, errors.Wrap(err, "failed to get unconsumed broadcasts min block number")
	}
	return blockNumber, nil
}

func (o *orm) removeUnconsumed(qopts ...postgres.QOpt) error {
	q := postgres.NewQ(o.db, qopts...)
	_, err := q.Exec(`
        DELETE FROM log_broadcasts
			WHERE evm_chain_id = $1
			AND consumed = false
			AND block_number IS NOT NULL
    `, o.evmChainID)
	return errors.Wrap(err, "failed to delete unconsumed broadcasts")
}

// LogBroadcast - gorm-compatible receive data from log_broadcasts table columns
type LogBroadcast struct {
	BlockHash common.Hash
	Consumed  bool
	LogIndex  uint
	JobID     int32
}

func (b LogBroadcast) AsKey() LogBroadcastAsKey {
	return LogBroadcastAsKey{
		b.BlockHash,
		b.LogIndex,
		b.JobID,
	}
}

// LogBroadcastAsKey - used as key in a map to filter out already consumed logs
type LogBroadcastAsKey struct {
	BlockHash common.Hash
	LogIndex  uint
	JobId     int32
}

func NewLogBroadcastAsKey(log types.Log, listener Listener) LogBroadcastAsKey {
	return LogBroadcastAsKey{
		log.BlockHash,
		log.Index,
		listener.JobID(),
	}
}
