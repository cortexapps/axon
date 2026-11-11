package axon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/cortexapps/axon-go/.generated/proto/github.com/cortexapps/axon"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// These tests run the SDK against a real agent built from ../../agent, and
// trigger invocations through its webhook endpoint.

var (
	agentBinOnce sync.Once
	agentBin     string
	agentBinErr  error
)

func buildAgent(t *testing.T) string {
	agentBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "axon-e2e")
		if err != nil {
			agentBinErr = err
			return
		}
		agentBin = filepath.Join(dir, "axon-agent")
		cmd := exec.Command("go", "build", "-o", agentBin, ".")
		cmd.Dir = filepath.Join("..", "..", "agent")
		out, err := cmd.CombinedOutput()
		if err != nil {
			agentBinErr = fmt.Errorf("building agent: %v\n%s", err, out)
		}
	})
	require.NoError(t, agentBinErr)
	return agentBin
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type realAgent struct {
	grpcPort    int
	webhookPort int
}

func startRealAgent(t *testing.T) *realAgent {
	if testing.Short() {
		t.Skip("starts a real agent")
	}
	bin := buildAgent(t)
	a := &realAgent{grpcPort: freePort(t), webhookPort: freePort(t)}

	cmd := exec.Command(bin, "serve")
	cmd.Env = append(os.Environ(),
		"DRYRUN=true",
		fmt.Sprintf("PORT=%d", a.grpcPort),
		fmt.Sprintf("HTTP_PORT=%d", freePort(t)),
		fmt.Sprintf("WEBHOOK_PORT=%d", a.webhookPort),
		"HANDLER_HISTORY_PATH="+t.TempDir(),
	)
	logFile, err := os.Create(filepath.Join(t.TempDir(), "agent.log"))
	require.NoError(t, err)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			logs, _ := os.ReadFile(logFile.Name())
			t.Logf("agent log:\n%s", logs)
		}
	})

	require.Eventually(t, func() bool {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", a.grpcPort))
		if err == nil {
			conn.Close()
		}
		return err == nil
	}, 30*time.Second, 50*time.Millisecond, "agent never started listening")
	return a
}

// postWebhook sends body once the webhook is live, and returns the status the
// agent accepted it with.
func (a *realAgent) postWebhook(t *testing.T, id, body string) int {
	url := fmt.Sprintf("http://127.0.0.1:%d/webhook/%s", a.webhookPort, id)
	var status int
	require.Eventually(t, func() bool {
		resp, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			return false
		}
		resp.Body.Close()
		status = resp.StatusCode
		// 404 until the SDK has registered and started dispatching.
		return status != http.StatusNotFound
	}, 10*time.Second, 50*time.Millisecond, "webhook never became available")
	return status
}

type e2eCall struct {
	info    InvocationInfo
	failure *Failure
}

// runAgainstRealAgent registers handler as webhook id on a fresh agent, with
// a failure hook that records each failure and asks for a retry.
func runAgainstRealAgent(t *testing.T, id string, handler Handler, opts ...RegisterHandlerOption) (*realAgent, chan e2eCall) {
	a := startRealAgent(t)
	failures := make(chan e2eCall, 10)

	logConfig := zap.NewDevelopmentConfig()
	logConfig.Level = zap.NewAtomicLevelAt(zap.WarnLevel)
	agent := NewAxonAgent(
		WithHostport("127.0.0.1", a.grpcPort),
		WithLoggerConfig(logConfig),
		WithFailureHook(func(_ context.Context, info InvocationInfo, failure *Failure) FailureAction {
			failures <- e2eCall{info, failure}
			return FailureActionRetry
		}),
	)

	opts = append(opts, WithInvokeOption(pb.HandlerInvokeType_WEBHOOK, id))
	_, err := agent.RegisterHandler(handler, opts...)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = agent.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	})
	return a, failures
}

type attempts struct {
	mu     sync.Mutex
	bodies []string
	ok     chan string
}

func newAttempts() *attempts { return &attempts{ok: make(chan string, 10)} }

func (r *attempts) record(ctx HandlerContext) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, ctx.Args()["body"])
	return len(r.bodies)
}

func requireFailure(t *testing.T, failures chan e2eCall) e2eCall {
	select {
	case call := <-failures:
		return call
	case <-time.After(15 * time.Second):
		require.FailNow(t, "failure hook never ran")
		return e2eCall{}
	}
}

func requireSucceeded(t *testing.T, r *attempts) string {
	select {
	case body := <-r.ok:
		return body
	case <-time.After(15 * time.Second):
		require.FailNow(t, "retry never succeeded")
		return ""
	}
}

func TestE2EHandlerErrorIsHookedAndRetried(t *testing.T) {
	r := newAttempts()
	a, failures := runAgainstRealAgent(t, "e2e-error", func(ctx HandlerContext) error {
		if r.record(ctx) == 1 {
			return fmt.Errorf("first attempt fails")
		}
		r.ok <- ctx.Args()["body"]
		return nil
	})

	require.Equal(t, http.StatusOK, a.postWebhook(t, "e2e-error", `{"id": 42}`))

	call := requireFailure(t, failures)
	require.Equal(t, pb.FailureReason_HANDLER_ERROR, call.failure.Reason)
	require.Equal(t, pb.FailureSource_APPLICATION, call.failure.Source)
	require.Equal(t, "first attempt fails", call.failure.Message)
	require.Equal(t, `{"id": 42}`, call.info.Args["body"])
	require.Equal(t, 1, call.info.Attempt)

	require.Equal(t, `{"id": 42}`, requireSucceeded(t, r), "the retry carries the original message")

	// It succeeded, so the hook does not run again.
	select {
	case extra := <-failures:
		require.FailNow(t, "unexpected second failure", "%+v", extra.failure)
	case <-time.After(time.Second):
	}
	r.mu.Lock()
	require.Len(t, r.bodies, 2)
	r.mu.Unlock()
}

func TestE2ETimeoutIsHookedAndRetried(t *testing.T) {
	r := newAttempts()
	a, failures := runAgainstRealAgent(t, "e2e-timeout", func(ctx HandlerContext) error {
		if r.record(ctx) == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		r.ok <- ctx.Args()["body"]
		return nil
	}, WithTimeout(500*time.Millisecond))

	require.Equal(t, http.StatusOK, a.postWebhook(t, "e2e-timeout", `{"id": 7}`))

	call := requireFailure(t, failures)
	require.Equal(t, pb.FailureReason_TIMEOUT, call.failure.Reason)
	require.Equal(t, pb.FailureSource_APPLICATION, call.failure.Source)
	require.Equal(t, `{"id": 7}`, call.info.Args["body"])

	require.Equal(t, `{"id": 7}`, requireSucceeded(t, r))

	// The SDK reported the timeout itself, so the agent must not report it
	// again once its grace period is over.
	select {
	case extra := <-failures:
		require.FailNow(t, "timeout reported twice", "%+v", extra.failure)
	case <-time.After(7 * time.Second):
	}
}
