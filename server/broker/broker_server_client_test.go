package broker

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func TestNewToken(t *testing.T) {
	token := NewToken("my-secret-token")
	assert.Equal(t, "my-secret-token", token.Raw())
	assert.Len(t, token.Hashed(), 64) // SHA-256 hex = 64 chars

	// Same input produces same hash.
	assert.Equal(t, token.Hashed(), NewToken("my-secret-token").Hashed())

	// Different input produces different hash.
	assert.NotEqual(t, token.Hashed(), NewToken("different-token").Hashed())
}

func TestTokenFromHash(t *testing.T) {
	token := TokenFromHash("abc123")
	assert.Equal(t, "", token.Raw())
	assert.Equal(t, "abc123", token.Hashed())
}

func TestClientConnected(t *testing.T) {
	var mu sync.Mutex
	var reqMethod, reqPath, reqContentType, reqQuery string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		reqMethod = r.Method
		reqPath = r.URL.Path
		reqContentType = r.Header.Get("Content-Type")
		reqQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := zaptest.NewLogger(t)
	client := NewClient(server.URL, "server-42", logger)

	token := NewToken("raw-token")
	err := client.ClientConnected(token, "client-123", map[string]string{"broker_client_version": "1.0"})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, http.MethodPost, reqMethod)
	assert.Equal(t, "/internal/brokerservers/server-42/connections/"+token.Hashed(), reqPath)
	assert.Equal(t, "application/vnd.api+json", reqContentType)
	assert.Contains(t, reqQuery, "broker_client_id=client-123")
	assert.Contains(t, reqQuery, "request_type=client-connected")
	assert.Contains(t, reqQuery, "version="+dispatcherAPIVersion)
}

func TestClientDisconnected(t *testing.T) {
	var reqMethod, reqPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqMethod = r.Method
		reqPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := zaptest.NewLogger(t)
	client := NewClient(server.URL, "server-42", logger)

	token := NewToken("raw-token")
	err := client.ClientDisconnected(token, "client-123")
	require.NoError(t, err)
	assert.Equal(t, http.MethodDelete, reqMethod)
	assert.Equal(t, "/internal/brokerservers/server-42/connections/"+token.Hashed(), reqPath)
}

func TestServerStarting(t *testing.T) {
	var reqMethod, reqPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqMethod = r.Method
		reqPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := zaptest.NewLogger(t)
	client := NewClient(server.URL, "server-42", logger)

	err := client.ServerStarting("my-hostname")
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, reqMethod)
	assert.Equal(t, "/internal/brokerservers/server-42", reqPath)
}

func TestServerStopping(t *testing.T) {
	var reqMethod, reqPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqMethod = r.Method
		reqPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := zaptest.NewLogger(t)
	client := NewClient(server.URL, "server-42", logger)

	err := client.ServerStopping()
	require.NoError(t, err)
	assert.Equal(t, http.MethodDelete, reqMethod)
	assert.Equal(t, "/internal/brokerservers/server-42", reqPath)
}

func TestNotConfigured(t *testing.T) {
	logger := zaptest.NewLogger(t)
	client := NewClient("", "server-42", logger)

	assert.False(t, client.IsConfigured())

	// All operations should be no-ops.
	assert.NoError(t, client.ClientConnected(NewToken("t"), "c", nil))
	assert.NoError(t, client.ClientDisconnected(NewToken("t"), "c"))
	assert.NoError(t, client.ServerStarting("host"))
	assert.NoError(t, client.ServerStopping())
}

func TestServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	logger := zaptest.NewLogger(t)
	client := NewClient(server.URL, "server-42", logger)

	err := client.ClientConnected(NewToken("token"), "client", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 500")
}
