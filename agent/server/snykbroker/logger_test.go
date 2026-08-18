package snykbroker

import (
	"bytes"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// testLogWriter forwards zap output to the test log, then goes quiet once the
// test finishes. Request goroutines outlive the test that started them, and
// writing to a finished test races tRunner marking it done. Waiting for them
// instead does not work: every layer logs after the one below it has already
// signalled completion.
//
// The mutex spans the check and the write, so a call cannot pass the check and
// then land after the test is done.
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

// newTestLogger returns a logger safe to call from goroutines that outlive the
// test. Use it instead of zaptest.NewLogger wherever one can.
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
