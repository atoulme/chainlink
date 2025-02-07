package pipeline_test

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/smartcontractkit/chainlink/core/internal/cltest"
	"github.com/smartcontractkit/sqlx"
)

func fakeExternalAdapter(t *testing.T, expectedRequest, response interface{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Helper()

		defer r.Body.Close()

		body, err := ioutil.ReadAll(r.Body)
		require.NoError(t, err)

		expectedBody := &bytes.Buffer{}
		err = json.NewEncoder(expectedBody).Encode(expectedRequest)
		require.NoError(t, err)
		require.Equal(t, bytes.TrimSpace(expectedBody.Bytes()), body)

		w.Header().Set("Content-Type", "application/json")
		err = json.NewEncoder(w).Encode(response)
		require.NoError(t, err)
	})
}

func makeBridge(t *testing.T, db *sqlx.DB, expectedRequest, response interface{}) (s *httptest.Server, name string) {
	t.Helper()

	server := httptest.NewServer(fakeExternalAdapter(t, expectedRequest, response))

	bridgeFeedURL, err := url.ParseRequestURI(server.URL)
	require.NoError(t, err)

	_, bt := cltest.MustCreateBridge(t, db, cltest.BridgeOpts{URL: bridgeFeedURL.String()})

	return server, bt.Name.String()
}
