package http

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cortexapps/axon/config"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// blockingHandler serves /block with a caller-supplied function, so a test can
// hold a request open across a Close.
type blockingHandler struct {
	serve func(w http.ResponseWriter, r *http.Request)
}

func (h *blockingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r)
}

func (h *blockingHandler) RegisterRoutes(m *mux.Router) error {
	m.Handle("/block", h)
	return nil
}

// newBlockingTestServer starts a server whose only route runs serve, and
// returns it with the port it bound.
func newBlockingTestServer(t *testing.T, serve func(w http.ResponseWriter, r *http.Request)) (*httpServer, int) {
	t.Helper()
	srv := NewHttpServer(HttpServerParams{
		Logger:   zap.NewNop(),
		Config:   config.AgentConfig{},
		Handlers: []RegisterableHandler{&blockingHandler{serve: serve}},
		Registry: prometheus.NewRegistry(),
	}).(*httpServer)
	port, err := srv.Start()
	require.NoError(t, err)
	return srv, port
}

// requestBlockPath fires a request at /block and discards the result. The
// caller synchronises on the handler rather than on this goroutine.
func requestBlockPath(port int) {
	go func() {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/block", port))
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// Close used to call http.Server.Close, which severs the connection but
// returns while handler goroutines are still running. Anything the handler
// touches on the way out - the request middleware logs after next.ServeHTTP
// returns - then ran after the caller believed the server was done with.
func TestCloseWaitsForInFlightRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var handlerFinished atomic.Bool

	srv, port := newBlockingTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
		handlerFinished.Store(true)
	})

	requestBlockPath(port)
	waitFor(t, entered, "the handler to be entered")

	// Released only after Close is already waiting, so a Close that does not
	// drain returns first and leaves handlerFinished false.
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(release)
	}()

	_ = srv.Close()

	require.True(t, handlerFinished.Load(),
		"Close returned while the in-flight handler was still running")
}

// The drain must be bounded: a handler that never returns cannot be allowed to
// wedge Close forever.
func TestCloseStopsWaitingAfterDrainTimeout(t *testing.T) {
	original := closeDrainTimeout
	closeDrainTimeout = 100 * time.Millisecond
	t.Cleanup(func() { closeDrainTimeout = original })

	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	srv, port := newBlockingTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	})

	requestBlockPath(port)
	waitFor(t, entered, "the handler to be entered")

	start := time.Now()
	_ = srv.Close()
	elapsed := time.Since(start)

	require.GreaterOrEqual(t, elapsed, 100*time.Millisecond,
		"Close should wait for the drain timeout before giving up")
	require.Less(t, elapsed, 3*time.Second,
		"Close should give up once the drain timeout expires, not wait for the handler")
}

// A server with nothing in flight must not pay the drain timeout.
func TestCloseReturnsPromptlyWhenIdle(t *testing.T) {
	entered := make(chan struct{})
	srv, port := newBlockingTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		w.WriteHeader(http.StatusOK)
	})

	requestBlockPath(port)
	waitFor(t, entered, "the handler to be entered")

	start := time.Now()
	_ = srv.Close()

	require.Less(t, time.Since(start), closeDrainTimeout,
		"Close should not wait out the drain timeout when no request is in flight")
}
