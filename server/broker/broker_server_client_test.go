package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	err := client.ClientConnected(context.Background(), token, "client-123", map[string]string{"broker_client_version": "1.0"})
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
	err := client.ClientDisconnected(context.Background(), token, "client-123")
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

	err := client.ServerStarting(context.Background(), "my-hostname")
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

	err := client.ServerStopping(context.Background())
	require.NoError(t, err)
	assert.Equal(t, http.MethodDelete, reqMethod)
	assert.Equal(t, "/internal/brokerservers/server-42", reqPath)
}

func TestNotConfigured(t *testing.T) {
	logger := zaptest.NewLogger(t)
	client := NewClient("", "server-42", logger)

	assert.False(t, client.IsConfigured())

	// All operations should be no-ops.
	assert.NoError(t, client.ClientConnected(context.Background(), NewToken("t"), "c", nil))
	assert.NoError(t, client.ClientDisconnected(context.Background(), NewToken("t"), "c"))
	assert.NoError(t, client.ServerStarting(context.Background(), "host"))
	assert.NoError(t, client.ServerStopping(context.Background()))
}

func TestServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	logger := zaptest.NewLogger(t)
	client := NewClient(server.URL, "server-42", logger)
	client.SetRetryPolicy(2, time.Millisecond) // 500 is transient; bound the test

	err := client.ClientConnected(context.Background(), NewToken("token"), "client", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 500")
}

func TestRetry_TransientThenSuccess(t *testing.T) {
	logger := zaptest.NewLogger(t)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(server.URL, "server-42", logger)
	client.SetRetryPolicy(4, time.Millisecond)

	err := client.ServerStarting(context.Background(), "host")
	require.NoError(t, err, "transient 5xx failures should be retried to success")
	assert.Equal(t, int32(3), attempts.Load())
}

func TestRetry_PermanentFailureNoRetry(t *testing.T) {
	logger := zaptest.NewLogger(t)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	client := NewClient(server.URL, "server-42", logger)
	client.SetRetryPolicy(4, time.Millisecond)

	err := client.ServerStarting(context.Background(), "host")
	require.Error(t, err)
	assert.Equal(t, int32(1), attempts.Load(), "4xx (non-429) must not be retried")
}

func TestRetry_ExhaustedReturnsLastError(t *testing.T) {
	logger := zaptest.NewLogger(t)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := NewClient(server.URL, "server-42", logger)
	client.SetRetryPolicy(3, time.Millisecond)

	err := client.ServerStarting(context.Background(), "host")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "after 3 attempts")
	assert.Equal(t, int32(3), attempts.Load())
}

func TestRetry_ContextCancelStopsRetrying(t *testing.T) {
	logger := zaptest.NewLogger(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	client := NewClient(server.URL, "server-42", logger)
	client.SetRetryPolicy(100, 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := client.ServerStarting(ctx, "host")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2*time.Second, "cancellation must stop the retry loop promptly")
}

func TestRetry_TooManyRequestsIsRetryable(t *testing.T) {
	logger := zaptest.NewLogger(t)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(server.URL, "server-42", logger)
	client.SetRetryPolicy(3, time.Millisecond)

	require.NoError(t, client.ServerStarting(context.Background(), "host"))
	assert.Equal(t, int32(2), attempts.Load())
}
