package snykbroker

import (
	"bytes"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// testLogWriter forwards zap output to the test log, then goes quiet once the
// test finishes.
//
// Goroutines started by a request outlive the test that started them: a tunnel
// copy goroutine logs from copyWithIdleTimeout, and requestMiddleware logs its
// "<== HTTP request" line only after ServeHTTP returns. Writing to a finished
// test races tRunner marking it done, which the race detector reports as a
// failure. In production the logger outlives every tunnel, so this is a harness
// lifetime mismatch rather than an agent bug.
//
// Waiting for the goroutines instead does not work: each layer finishes logging
// after the layer below it has already signalled completion, so any counter is
// released while the layers above it are still logging.
//
// The mutex covers the check and the write together, so a log call cannot pass
// the check and then land after the test has been marked done.
type testLogWriter struct {
	mu       sync.Mutex
	t        testing.TB
	finished bool
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.finished {
		w.t.Logf("%s", bytes.TrimRight(p, "\n"))
	}
	return len(p), nil
}

func (w *testLogWriter) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.finished = true
}

// newTestLogger returns a logger that is safe to call from goroutines which
// outlive the test. Use it instead of zaptest.NewLogger anywhere a request or
// tunnel goroutine can still be running at teardown.
func newTestLogger(t testing.TB) *zap.Logger {
	t.Helper()
	w := &testLogWriter{t: t}
	t.Cleanup(w.stop)
	return zap.New(zapcore.NewCore(
		zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig()),
		zapcore.AddSync(w),
		zapcore.DebugLevel,
	))
}
