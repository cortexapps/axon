package server

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/cron"
	"github.com/cortexapps/axon/server/handler"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

const failureTestWebhook = "failure-test-hook"

type failureEventsFixture struct {
	t       *testing.T
	ctx     context.Context
	manager handler.Manager
	client  *testAxonClient

	mu       sync.Mutex
	invokes  []*pb.DispatchHandlerInvoke
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
	agent := NewAxonAgent(Params{Logger: logger, Config: cfg, Manager: manager})
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

func (f *failureEventsFixture) events() <-chan *pb.DispatchEvent {
	stream, err := f.client.DispatchEvents(f.ctx)
	require.NoError(f.t, err)
	out := make(chan *pb.DispatchEvent, 10)
	go func() {
		for {
			ev, err := stream.Recv()
			if err != nil {
				return
			}
			out <- ev
		}
	}()
	return out
}

func requireEvent(t *testing.T, events <-chan *pb.DispatchEvent) *pb.DispatchEvent {
	select {
	case ev := <-events:
		return ev
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no failure event arrived")
		return nil
	}
}

func requireNoEvent(t *testing.T, events <-chan *pb.DispatchEvent, wait time.Duration) {
	select {
	case ev := <-events:
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

	events := f.events()
	ev := requireEvent(t, events)
	require.Equal(t, first.InvocationId, ev.Invoke.InvocationId)
	require.Equal(t, `{"id": 1}`, ev.Invoke.Args["body"])
	require.Equal(t, pb.FailureReason_TIMEOUT, ev.Failure.Reason)
	require.Equal(t, pb.FailureSource_RUNTIME, ev.Failure.Source)

	resp, err := f.client.RetryInvocation(f.ctx, ev.Invoke)
	require.NoError(t, err)
	require.Nil(t, resp.Error)

	retried := f.nextInvoke()
	require.Equal(t, resp.InvocationId, retried.InvocationId)
	require.Equal(t, int32(2), retried.Attempt)
	require.Equal(t, `{"id": 1}`, retried.Args["body"])

	// The retry succeeded, so nothing else fails.
	requireNoEvent(t, events, time.Second)
}

func TestFailureEvents_ClientReportedFailureIsNotRepeated(t *testing.T) {
	f := startFailureEventsFixture(t, 200*time.Millisecond, func(*pb.DispatchHandlerInvoke) (clientResult, error) {
		// The client saw the failure and fires its own hook.
		return clientResult{}, errors.New("handler blew up")
	})
	events := f.events()

	f.postWebhook(`{}`)
	f.nextInvoke()

	// Past the timeout and grace, the agent must stay quiet.
	requireNoEvent(t, events, time.Second)
}

func TestFailureEvents_RetryRejectedAtLimit(t *testing.T) {
	f := startFailureEventsFixture(t, time.Second, func(*pb.DispatchHandlerInvoke) (clientResult, error) {
		return clientResult{result: "ok"}, nil
	})

	f.postWebhook(`{}`)
	invoke := proto.Clone(f.nextInvoke()).(*pb.DispatchHandlerInvoke)
	invoke.Attempt = 5

	resp, err := f.client.RetryInvocation(f.ctx, invoke)
	require.NoError(t, err)
	require.NotNil(t, resp.Error)
	require.Equal(t, "rejected", resp.Error.Code)
}
