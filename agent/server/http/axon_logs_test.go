package http

import (
	"encoding/base64"
	"encoding/json"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/logstore"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// __axon/logs pages through the agent's captured log output so the backend
// can read it over the relay. It is served only when REMOTE_LOGS_VISIBLE
// turned capture on; otherwise it is a 404, not an empty page, so the
// backend can tell "off" from "nothing logged".

func logsServer(t *testing.T, store *logstore.Store) *httptest.Server {
	t.Helper()
	logger, _ := zap.NewDevelopment()
	h := NewAxonHandler(AxonHandlerParams{Logger: logger, Config: config.AgentConfig{}, LogStore: store})
	router := mux.NewRouter()
	require.NoError(t, h.RegisterRoutes(router))
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func getLogs(t *testing.T, srv *httptest.Server, query string) (int, map[string]any, nethttp.Header) {
	t.Helper()
	resp, err := nethttp.Get(srv.URL + "/__axon/logs" + query)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(body, &parsed), "body is not JSON: %s", body)
	return resp.StatusCode, parsed, resp.Header
}

func TestLogs_NotFoundWhenNoStore(t *testing.T) {
	srv := logsServer(t, nil)
	status, body, headers := getLogs(t, srv, "")
	assert.Equal(t, nethttp.StatusNotFound, status)
	assert.Equal(t, "application/json", headers.Get("Content-Type"))
	assert.Contains(t, body["error"], "not enabled")
}

func TestLogs_NotFoundWhenStoreDisabled(t *testing.T) {
	srv := logsServer(t, logstore.Disabled())
	status, _, _ := getLogs(t, srv, "")
	assert.Equal(t, nethttp.StatusNotFound, status)
}

func TestLogs_RejectsNonGet(t *testing.T) {
	srv := logsServer(t, openTestStore(t))
	resp, err := nethttp.Post(srv.URL+"/__axon/logs", "application/json", nil)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, nethttp.StatusMethodNotAllowed, resp.StatusCode)
}

func TestLogs_RejectsBadQuery(t *testing.T) {
	srv := logsServer(t, openTestStore(t))
	badToken := base64.RawURLEncoding.EncodeToString([]byte("1:2:3"))
	for _, q := range []string{"?size=0", "?size=10001", "?size=big", "?next=garbage", "?next=" + badToken} {
		status, body, _ := getLogs(t, srv, q)
		assert.Equal(t, nethttp.StatusBadRequest, status, q)
		assert.NotEmpty(t, body["error"], q)
	}
}

func TestLogs_EmptyStoreIsAnEmptyPage(t *testing.T) {
	srv := logsServer(t, openTestStore(t))
	status, body, headers := getLogs(t, srv, "")
	require.Equal(t, nethttp.StatusOK, status)
	assert.Equal(t, "application/json", headers.Get("Content-Type"))
	assert.Equal(t, []any{}, body["lines"], "lines is an array even when empty")
	assert.NotContains(t, body, "next", "no continuation when nothing is older")
}

func TestLogs_DefaultPageSizeIs1000(t *testing.T) {
	store := openTestStore(t)
	logger := zap.New(zapcore.NewNopCore(), store.ZapOption())
	for i := 1; i <= 1001; i++ {
		logger.Info("entry", zap.Int("i", i))
	}
	srv := logsServer(t, store)

	status, body, _ := getLogs(t, srv, "")
	require.Equal(t, nethttp.StatusOK, status)
	assert.Len(t, body["lines"], 1000)
	assert.NotEmpty(t, body["next"])
	assert.Equal(t, 2, entryIndexes(t, body)[0])

	status, body, _ = getLogs(t, srv, "?size=10000")
	require.Equal(t, nethttp.StatusOK, status)
	assert.Len(t, body["lines"], 1001)
	assert.NotContains(t, body, "next")
}

func TestLogs_PagesBackwardsWithContinuationToken(t *testing.T) {
	store := openTestStore(t)
	clock := &stepClock{now: time.Unix(1_000, 0)}
	logger := zap.New(zapcore.NewNopCore(), zap.WithClock(clock), store.ZapOption())
	for i := 1; i <= 7; i++ {
		clock.now = time.Unix(1_000+int64(i), 0)
		logger.Info("entry", zap.Int("i", i))
	}
	srv := logsServer(t, store)

	status, p0, _ := getLogs(t, srv, "?size=3")
	require.Equal(t, nethttp.StatusOK, status)
	assert.Equal(t, []int{5, 6, 7}, entryIndexes(t, p0))
	next, _ := p0["next"].(string)
	require.NotEmpty(t, next)

	status, p1, _ := getLogs(t, srv, "?size=3&next="+next)
	require.Equal(t, nethttp.StatusOK, status)
	assert.Equal(t, []int{2, 3, 4}, entryIndexes(t, p1))
	next, _ = p1["next"].(string)
	require.NotEmpty(t, next)

	status, p2, _ := getLogs(t, srv, "?size=3&next="+next)
	require.Equal(t, nethttp.StatusOK, status)
	assert.Equal(t, []int{1}, entryIndexes(t, p2))
	assert.NotContains(t, p2, "next")
}

// Each line is handed back as the JSON document the store holds, as a string.
func TestLogs_LinesAreStructuredJSONStrings(t *testing.T) {
	store := openTestStore(t)
	zap.New(zapcore.NewNopCore(), store.ZapOption()).Named("axon").Warn("something odd", zap.String("k", "v"))
	srv := logsServer(t, store)

	status, body, _ := getLogs(t, srv, "")
	require.Equal(t, nethttp.StatusOK, status)
	lines := body["lines"].([]any)
	require.Len(t, lines, 1)
	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0].(string)), &entry))
	assert.Equal(t, "warn", entry["level"])
	assert.Equal(t, "axon", entry["logger"])
	assert.Equal(t, "something odd", entry["msg"])
	assert.Equal(t, "v", entry["k"])
}

func openTestStore(t *testing.T) *logstore.Store {
	t.Helper()
	s, err := logstore.Open(logstore.Options{Path: t.TempDir(), MaxSizeBytes: 1 << 20, Level: zapcore.DebugLevel})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func entryIndexes(t *testing.T, body map[string]any) []int {
	t.Helper()
	out := []int{}
	for _, l := range body["lines"].([]any) {
		var e struct {
			I int `json:"i"`
		}
		require.NoError(t, json.Unmarshal([]byte(l.(string)), &e))
		out = append(out, e.I)
	}
	return out
}

type stepClock struct{ now time.Time }

func (c *stepClock) Now() time.Time                         { return c.now }
func (c *stepClock) NewTicker(d time.Duration) *time.Ticker { return time.NewTicker(d) }
