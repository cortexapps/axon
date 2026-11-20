package server

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/cron"
	"github.com/cortexapps/axon/server/handler"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const invocationFailureWebhook = "invocation-failure-hook"

// recordingNotifier collects the failures the manager hands on.
type recordingNotifier struct {
	failures chan *pb.DispatchEvent
}

func (r *recordingNotifier) Notify(invoke *pb.DispatchHandlerInvoke, failure *pb.InvocationFailure) {
	// Never block the manager's watcher. A test reads the failures it
	// expects, so one past the buffer is one no test is waiting for.
	select {
	case r.failures <- &pb.DispatchEvent{Invoke: invoke, Failure: failure}:
	default:
	}
}

type invocationFailureFixture struct {
	t        *testing.T
	ctx      context.Context
	manager  handler.Manager
	client   *testAxonClient
	notifier *recordingNotifier
	received chan *pb.DispatchHandlerInvoke
}

// startInvocationFailureFixture runs an agent and a client with one webhook
// handler. respond decides what the client does with each invocation.
func startInvocationFailureFixture(t *testing.T, timeout time.Duration, respond func(*pb.DispatchHandlerInvoke) (clientResult, error)) *invocationFailureFixture {
	oldGrace := reportGrace
	reportGrace = 100 * time.Millisecond
	t.Cleanup(func() { reportGrace = oldGrace })

	port := getRandomPort()
	cfg := config.AgentConfig{
		GrpcPort:           port,
		CortexApiBaseUrl:   "http://localhost",
		CortexApiToken:     "test-token",
		DequeueWaitTime:    100 * time.Millisecond,
		HandlerHistoryPath: filepath.Join(t.TempDir(), "history"),
	}

	logger, _ := zap.NewDevelopment()
	manager := handler.NewHandlerManager(logger, cron.New(), nil)
	notifier := &recordingNotifier{failures: make(chan *pb.DispatchEvent, 10)}
	manager.SetFailureNotifier(notifier)
	agent := NewAxonAgent(Params{Logger: logger, Config: cfg, Manager: manager})
	require.NoError(t, agent.Start(context.Background()))
	t.Cleanup(agent.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	f := &invocationFailureFixture{
		t:        t,
		ctx:      ctx,
		manager:  manager,
		notifier: notifier,
		received: make(chan *pb.DispatchHandlerInvoke, 10),
	}

	f.client = NewTestAxonClient(t, int32(port), func(msg *pb.DispatchMessage) (clientResult, error) {
		invoke := msg.GetInvoke()
		if invoke == nil {
			return clientResult{}, nil
		}
		f.received <- invoke
		return respond(invoke)
	})
	t.Cleanup(f.client.Close)

	_, err := f.client.client.RegisterHandler(ctx, &pb.RegisterHandlerRequest{
		DispatchId:  f.client.dispatchId,
		HandlerName: "hook",
		TimeoutMs:   int32(timeout.Milliseconds()),
		Options: []*pb.HandlerOption{{
			Option: &pb.HandlerOption_Invoke{
				Invoke: &pb.HandlerInvokeOption{Type: pb.HandlerInvokeType_WEBHOOK, Value: invocationFailureWebhook},
			},
		}},
	})
	require.NoError(t, err)

	go f.client.Run(ctx)
	require.Eventually(t, func() bool { return manager.GetByTag(invocationFailureWebhook) != nil },
		5*time.Second, 10*time.Millisecond, "handler never started")
	return f
}

func (f *invocationFailureFixture) postWebhook(body string) {
	u, _ := url.Parse("http://localhost/webhook/" + invocationFailureWebhook)
	entry := f.manager.GetByTag(invocationFailureWebhook)
	require.NoError(f.t, f.manager.Trigger(handler.NewWebhookHandlerInvoke(entry, u, body, "application/json")))
}

func (f *invocationFailureFixture) nextInvoke() *pb.DispatchHandlerInvoke {
	select {
	case invoke := <-f.received:
		return invoke
	case <-time.After(5 * time.Second):
		require.FailNow(f.t, "client never received an invocation")
		return nil
	}
}

func (f *invocationFailureFixture) nextFailure() *pb.DispatchEvent {
	select {
	case ev := <-f.notifier.failures:
		return ev
	case <-time.After(5 * time.Second):
		require.FailNow(f.t, "no failure was reported")
		return nil
	}
}

func (f *invocationFailureFixture) requireNoFailure(wait time.Duration) {
	select {
	case ev := <-f.notifier.failures:
		require.FailNow(f.t, "unexpected failure", "%v", ev)
	case <-time.After(wait):
	}
}

func TestUnreportedInvocationIsAnAgentTimeout(t *testing.T) {
	f := startInvocationFailureFixture(t, 200*time.Millisecond, func(*pb.DispatchHandlerInvoke) (clientResult, error) {
		return clientResult{noReport: true}, nil
	})

	before := time.Now()
	f.postWebhook(`{"id": 1}`)
	invoke := f.nextInvoke()
	require.Equal(t, int32(1), invoke.Attempt)
	// The deadline is the timeout plus the grace, from when it was sent.
	require.NotNil(t, invoke.Deadline)
	require.WithinRange(t, invoke.Deadline.AsTime(), before.Add(300*time.Millisecond), time.Now().Add(300*time.Millisecond))

	ev := f.nextFailure()
	require.Equal(t, invoke.InvocationId, ev.Invoke.InvocationId)
	require.Equal(t, `{"id": 1}`, ev.Invoke.Args["body"])
	require.Equal(t, pb.FailureReason_TIMEOUT, ev.Failure.Reason)
	require.Equal(t, pb.FailureSource_RUNTIME, ev.Failure.Source)
}

func TestClientReportedFailureIsHandedOnOnce(t *testing.T) {
	f := startInvocationFailureFixture(t, 200*time.Millisecond, func(*pb.DispatchHandlerInvoke) (clientResult, error) {
		return clientResult{failure: &pb.InvocationFailure{
			Reason:  pb.FailureReason_HANDLER_ERROR,
			Source:  pb.FailureSource_APPLICATION,
			Message: "handler blew up",
		}}, errors.New("handler blew up")
	})

	f.postWebhook(`{}`)
	invoke := f.nextInvoke()

	ev := f.nextFailure()
	require.Equal(t, invoke.InvocationId, ev.Invoke.InvocationId)
	require.Equal(t, pb.FailureReason_HANDLER_ERROR, ev.Failure.Reason)
	require.Equal(t, pb.FailureSource_APPLICATION, ev.Failure.Source)

	// Past the timeout and grace, the agent must not report it again.
	f.requireNoFailure(time.Second)
}

func TestClientErrorWithoutFailureIsOnlyRecorded(t *testing.T) {
	f := startInvocationFailureFixture(t, 200*time.Millisecond, func(*pb.DispatchHandlerInvoke) (clientResult, error) {
		// An older client reports an error without classifying it.
		return clientResult{}, errors.New("handler blew up")
	})

	f.postWebhook(`{}`)
	f.nextInvoke()

	f.requireNoFailure(time.Second)
}

func TestDispatchEventsIsUnimplemented(t *testing.T) {
	f := startInvocationFailureFixture(t, 200*time.Millisecond, func(*pb.DispatchHandlerInvoke) (clientResult, error) {
		return clientResult{result: "ok"}, nil
	})

	stream, err := f.client.client.DispatchEvents(f.ctx)
	require.NoError(t, err)
	// The agent may end the stream before this arrives; Recv has its answer.
	_ = stream.Send(&pb.DispatchEventsRequest{DispatchId: f.client.dispatchId})
	_, err = stream.Recv()
	require.Equal(t, codes.Unimplemented, status.Code(err))
}
