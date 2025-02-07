package keystore

import (
	"database/sql"

	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/services/keystore/keys/csakey"
	"github.com/smartcontractkit/chainlink/core/services/keystore/keys/ethkey"
	"github.com/smartcontractkit/chainlink/core/services/keystore/keys/ocrkey"
	"github.com/smartcontractkit/chainlink/core/services/keystore/keys/p2pkey"
	"github.com/smartcontractkit/chainlink/core/services/keystore/keys/vrfkey"
	"github.com/smartcontractkit/chainlink/core/services/postgres"

	"github.com/pkg/errors"
	"github.com/smartcontractkit/sqlx"
)

func NewORM(db *sqlx.DB, lggr logger.Logger) ksORM {
	return ksORM{
		db:   db,
		lggr: lggr.Named("KeystoreORM"),
	}
}

type ksORM struct {
	db   *sqlx.DB
	lggr logger.Logger
}

func (orm ksORM) saveEncryptedKeyRing(kr *encryptedKeyRing, callbacks ...func(postgres.Queryer) error) error {
	return postgres.NewQ(orm.db).Transaction(orm.lggr, func(tx postgres.Queryer) error {
		_, err := tx.Exec(`
		UPDATE encrypted_key_rings
		SET encrypted_keys = $1
	`, kr.EncryptedKeys)
		if err != nil {
			return errors.Wrap(err, "while saving keyring")
		}
		for _, callback := range callbacks {
			err = callback(tx)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (orm ksORM) getEncryptedKeyRing() (kr encryptedKeyRing, err error) {
	err = orm.db.Get(&kr, `SELECT * FROM encrypted_key_rings LIMIT 1`)
	if errors.Is(err, sql.ErrNoRows) {
		sql := `INSERT INTO encrypted_key_rings (encrypted_keys, updated_at) VALUES (NULL, NOW()) RETURNING *;`
		err2 := orm.db.Get(&kr, sql)

		if err2 != nil {
			return kr, err2
		}
	} else if err != nil {
		return kr, err
	}
	return kr, nil
}

func (orm ksORM) loadKeyStates() (keyStates, error) {
	ks := newKeyStates()
	var ethkeystates []ethkey.State
	if err := orm.db.Select(&ethkeystates, `SELECT * FROM eth_key_states`); err != nil {
		return ks, errors.Wrap(err, "error loading eth_key_states from DB")
	}
	for i := 0; i < len(ethkeystates); i++ {
		ks.Eth[ethkeystates[i].KeyID()] = &ethkeystates[i]
	}
	return ks, nil
}

// ~~~~~~~~~~~~~~~~~~~~ LEGACY FUNCTIONS FOR V1 MIGRATION ~~~~~~~~~~~~~~~~~~~~

func (orm ksORM) GetEncryptedV1CSAKeys() (retrieved []csakey.Key, err error) {
	return retrieved, orm.db.Select(&retrieved, `SELECT * FROM csa_keys`)
}

func (orm ksORM) GetEncryptedV1EthKeys() (retrieved []ethkey.Key, err error) {
	return retrieved, orm.db.Select(&retrieved, `SELECT * FROM keys`)
}

func (orm ksORM) GetEncryptedV1OCRKeys() (retrieved []ocrkey.EncryptedKeyBundle, err error) {
	return retrieved, orm.db.Select(&retrieved, `SELECT * FROM encrypted_ocr_key_bundles`)
}

func (orm ksORM) GetEncryptedV1P2PKeys() (retrieved []p2pkey.EncryptedP2PKey, err error) {
	return retrieved, orm.db.Select(&retrieved, `SELECT * FROM encrypted_p2p_keys`)
}

func (orm ksORM) GetEncryptedV1VRFKeys() (retrieved []vrfkey.EncryptedVRFKey, err error) {
	return retrieved, orm.db.Select(&retrieved, `SELECT * FROM encrypted_vrf_keys`)
}
