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
	"google.golang.org/grpc"
)

const failureTestWebhook = "failure-test-hook"

type failureEventsFixture struct {
	t        *testing.T
	ctx      context.Context
	manager  handler.Manager
	client   *testAxonClient
	received chan *pb.DispatchHandlerInvoke
}

// startFailureEventsFixture runs an agent and a client with one webhook
// handler. respond decides what the client does with each invocation.
func startFailureEventsFixture(t *testing.T, timeout time.Duration, respond func(*pb.DispatchHandlerInvoke) (clientResult, error)) *failureEventsFixture {
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
	failures := handler.NewFailureManager(logger, nil, manager)
	agent := NewAxonAgent(Params{Logger: logger, Config: cfg, Manager: manager, Failures: failures})
	require.NoError(t, agent.Start(context.Background()))
	t.Cleanup(agent.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	f := &failureEventsFixture{
		t:        t,
		ctx:      ctx,
		manager:  manager,
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
	f.client.failureEvents = true
	t.Cleanup(f.client.Close)

	_, err := f.client.client.RegisterHandler(ctx, &pb.RegisterHandlerRequest{
		DispatchId:  f.client.dispatchId,
		HandlerName: "hook",
		TimeoutMs:   int32(timeout.Milliseconds()),
		Options: []*pb.HandlerOption{{
			Option: &pb.HandlerOption_Invoke{
				Invoke: &pb.HandlerInvokeOption{Type: pb.HandlerInvokeType_WEBHOOK, Value: failureTestWebhook},
			},
		}},
	})
	require.NoError(t, err)

	go f.client.Run(ctx)
	require.Eventually(t, func() bool { return manager.GetByTag(failureTestWebhook) != nil },
		5*time.Second, 10*time.Millisecond, "handler never started")
	return f
}

func (f *failureEventsFixture) postWebhook(body string) {
	u, _ := url.Parse("http://localhost/webhook/" + failureTestWebhook)
	entry := f.manager.GetByTag(failureTestWebhook)
	require.NoError(f.t, f.manager.Trigger(handler.NewWebhookHandlerInvoke(entry, u, body, "application/json")))
}

func (f *failureEventsFixture) nextInvoke() *pb.DispatchHandlerInvoke {
	select {
	case invoke := <-f.received:
		return invoke
	case <-time.After(5 * time.Second):
		require.FailNow(f.t, "client never received an invocation")
		return nil
	}
}

type eventStream struct {
	stream grpc.BidiStreamingClient[pb.DispatchEventsRequest, pb.DispatchEvent]
	events chan *pb.DispatchEvent
}

// events opens the failure event stream, as a client with a failure hook
// does. ctx bounds the stream, so cancelling it simulates a disconnect.
func (f *failureEventsFixture) events(ctx context.Context) *eventStream {
	stream, err := f.client.DispatchEvents(ctx)
	require.NoError(f.t, err)
	s := &eventStream{stream: stream, events: make(chan *pb.DispatchEvent, 10)}
	go func() {
		for {
			ev, err := stream.Recv()
			if err != nil {
				return
			}
			s.events <- ev
		}
	}()
	return s
}

func (s *eventStream) resolve(t *testing.T, ev *pb.DispatchEvent, action pb.FailureAction) {
	require.NoError(t, s.stream.Send(&pb.DispatchEventsRequest{
		Resolution: &pb.FailureResolution{InvocationId: ev.Invoke.InvocationId, Action: action},
	}))
}

func requireEvent(t *testing.T, s *eventStream) *pb.DispatchEvent {
	select {
	case ev := <-s.events:
		return ev
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no failure event arrived")
		return nil
	}
}

func requireNoEvent(t *testing.T, s *eventStream, wait time.Duration) {
	select {
	case ev := <-s.events:
		require.FailNow(t, "unexpected failure event", "%v", ev)
	case <-time.After(wait):
	}
}

func TestFailureEvents_UnreportedInvocationTimesOutAndRetries(t *testing.T) {
	calls := 0
	f := startFailureEventsFixture(t, 200*time.Millisecond, func(*pb.DispatchHandlerInvoke) (clientResult, error) {
		calls++
		if calls == 1 {
			return clientResult{noReport: true}, nil
		}
		return clientResult{result: "ok"}, nil
	})

	// Connect after the failure so delivery doesn't depend on a live stream.
	f.postWebhook(`{"id": 1}`)
	first := f.nextInvoke()
	require.Equal(t, int32(1), first.Attempt)
	time.Sleep(500 * time.Millisecond)

	events := f.events(f.ctx)
	ev := requireEvent(t, events)
	require.Equal(t, first.InvocationId, ev.Invoke.InvocationId)
	require.Equal(t, `{"id": 1}`, ev.Invoke.Args["body"])
	require.Equal(t, pb.FailureReason_TIMEOUT, ev.Failure.Reason)
	require.Equal(t, pb.FailureSource_RUNTIME, ev.Failure.Source)

	events.resolve(t, ev, pb.FailureAction_FAILURE_ACTION_RETRY)

	retried := f.nextInvoke()
	require.NotEqual(t, first.InvocationId, retried.InvocationId)
	require.Equal(t, int32(2), retried.Attempt)
	require.Equal(t, `{"id": 1}`, retried.Args["body"])

	// The retry succeeded, so nothing else fails.
	requireNoEvent(t, events, time.Second)
}

func TestFailureEvents_ClientReportedFailureIsDeliveredOnce(t *testing.T) {
	f := startFailureEventsFixture(t, 200*time.Millisecond, func(*pb.DispatchHandlerInvoke) (clientResult, error) {
		return clientResult{failure: &pb.InvocationFailure{
			Reason:  pb.FailureReason_HANDLER_ERROR,
			Source:  pb.FailureSource_APPLICATION,
			Message: "handler blew up",
		}}, errors.New("handler blew up")
	})
	events := f.events(f.ctx)

	f.postWebhook(`{}`)
	invoke := f.nextInvoke()

	ev := requireEvent(t, events)
	require.Equal(t, invoke.InvocationId, ev.Invoke.InvocationId)
	require.Equal(t, pb.FailureReason_HANDLER_ERROR, ev.Failure.Reason)
	require.Equal(t, pb.FailureSource_APPLICATION, ev.Failure.Source)
	events.resolve(t, ev, pb.FailureAction_FAILURE_ACTION_DROP)

	// Past the timeout and grace, the agent must not report it again, and
	// dropping it must not run it again.
	requireNoEvent(t, events, time.Second)
	select {
	case again := <-f.received:
		require.FailNow(t, "dropped invocation ran again", "%v", again)
	default:
	}
}

func TestFailureEvents_UnresolvedEventIsResentOnReconnect(t *testing.T) {
	f := startFailureEventsFixture(t, 200*time.Millisecond, func(*pb.DispatchHandlerInvoke) (clientResult, error) {
		return clientResult{noReport: true}, nil
	})

	f.postWebhook(`{}`)
	invoke := f.nextInvoke()

	// The client receives the failure but disconnects before answering.
	firstCtx, disconnect := context.WithCancel(f.ctx)
	ev := requireEvent(t, f.events(firstCtx))
	require.Equal(t, invoke.InvocationId, ev.Invoke.InvocationId)
	disconnect()

	again := requireEvent(t, f.events(f.ctx))
	require.Equal(t, invoke.InvocationId, again.Invoke.InvocationId)
}
