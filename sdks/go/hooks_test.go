package axon

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	pb "github.com/cortexapps/axon-go/.generated/proto/github.com/cortexapps/axon"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type hookCall struct {
	info    InvocationInfo
	failure *Failure
}

// hookRecorder is a post-dispatch hook that records each call.
type hookRecorder struct {
	mu     sync.Mutex
	calls  []hookCall
	action PostAction
	seen   chan struct{}
}

func newHookRecorder(action PostAction) *hookRecorder {
	return &hookRecorder{action: action, seen: make(chan struct{}, 10)}
}

func (r *hookRecorder) hook(_ context.Context, info InvocationInfo, failure *Failure) PostAction {
	r.mu.Lock()
	r.calls = append(r.calls, hookCall{info, failure})
	r.mu.Unlock()
	r.seen <- struct{}{}
	return r.action
}

func (r *hookRecorder) only(t *testing.T) hookCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Len(t, r.calls, 1)
	return r.calls[0]
}

// eventStream is a DispatchEvents stream that delivers events and then
// blocks until the agent stops reading.
type eventStream struct {
	grpc.ClientStream
	ctx    context.Context
	events chan *pb.DispatchEvent
	err    error
}

func (s *eventStream) Recv() (*pb.DispatchEvent, error) {
	if s.err != nil {
		return nil, s.err
	}
	select {
	case ev := <-s.events:
		return ev, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func expectEventStream(mock *mockGrpcClient, events ...*pb.DispatchEvent) {
	mock.agentStub.EXPECT().DispatchEvents(gomock.Any(), gomock.Any()).AnyTimes().DoAndReturn(
		func(ctx context.Context, req *pb.DispatchRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[pb.DispatchEvent], error) {
			ch := make(chan *pb.DispatchEvent, len(events))
			for _, ev := range events {
				ch <- ev
			}
			events = nil
			return &eventStream{ctx: ctx, events: ch}, nil
		})
}

func runWithPostHook(t *testing.T, handler any, timeoutMs int, rec *hookRecorder, setup ...func(*mockGrpcClient)) {
	controller := gomock.NewController(t)
	defer controller.Finish()
	executeHandlerWithAgent(t, controller, handler, timeoutMs, nil, func(agent *Agent, mock *mockGrpcClient) {
		agent.postDispatch = rec.hook
		expectEventStream(mock)
		for _, s := range setup {
			s(mock)
		}
	})
}

func TestPostDispatchHookOnSuccess(t *testing.T) {
	rec := newHookRecorder(PostActionRetry)
	runWithPostHook(t, func(ctx HandlerContext) error { return nil }, 0, rec)

	call := rec.only(t)
	require.Nil(t, call.failure)
	require.Equal(t, "func1", call.info.HandlerName)
	require.Equal(t, pb.HandlerInvokeType_RUN_NOW, call.info.InvokeType)
	require.Equal(t, 1, call.info.Attempt, "an agent that doesn't send attempt means the first")
	// Retry on success is ignored: no RetryInvocation expectation is set.
}

func TestPostDispatchHookOnHandlerError(t *testing.T) {
	rec := newHookRecorder(PostActionIgnore)
	runWithPostHook(t, func(ctx HandlerContext) error { return fmt.Errorf("ohnoes") }, 0, rec)

	failure := rec.only(t).failure
	require.NotNil(t, failure)
	require.Equal(t, pb.FailureReason_HANDLER_ERROR, failure.Reason)
	require.Equal(t, pb.FailureSource_APPLICATION, failure.Source)
	require.Equal(t, "ohnoes", failure.Message)
}

func TestPostDispatchHookOnPanic(t *testing.T) {
	rec := newHookRecorder(PostActionIgnore)
	runWithPostHook(t, func(ctx HandlerContext) error { panic("ohnoes") }, 0, rec)

	failure := rec.only(t).failure
	require.Equal(t, pb.FailureReason_HANDLER_ERROR, failure.Reason)
	require.Equal(t, pb.FailureSource_APPLICATION, failure.Source)
}

func TestPostDispatchHookOnHandlerTimeout(t *testing.T) {
	rec := newHookRecorder(PostActionIgnore)
	runWithPostHook(t, func(ctx HandlerContext) error {
		time.Sleep(50 * time.Millisecond)
		return nil
	}, 1, rec)

	failure := rec.only(t).failure
	require.Equal(t, pb.FailureReason_TIMEOUT, failure.Reason)
	require.Equal(t, pb.FailureSource_APPLICATION, failure.Source)
}

func TestPostDispatchHookTimeoutDuringApiCallIsRuntime(t *testing.T) {
	rec := newHookRecorder(PostActionIgnore)
	runWithPostHook(t, func(ctx HandlerContext) error {
		_, err := ctx.CortexJsonApiCall("GET", "/api/v1/slow", "")
		return err
	}, 20, rec, func(mock *mockGrpcClient) {
		mock.apiStub.EXPECT().Call(gomock.Any(), gomock.Any()).DoAndReturn(
			func(ctx context.Context, req *pb.CallRequest, opts ...grpc.CallOption) (*pb.CallResponse, error) {
				<-ctx.Done()
				return nil, status.FromContextError(ctx.Err()).Err()
			})
	})

	failure := rec.only(t).failure
	require.Equal(t, pb.FailureReason_TIMEOUT, failure.Reason)
	require.Equal(t, pb.FailureSource_RUNTIME, failure.Source)
}

func TestPostDispatchHookRetry(t *testing.T) {
	rec := newHookRecorder(PostActionRetry)
	retried := make(chan *pb.RetryInvocationRequest, 1)
	runWithPostHook(t, func(ctx HandlerContext) error { return fmt.Errorf("ohnoes") }, 0, rec, func(mock *mockGrpcClient) {
		mock.agentStub.EXPECT().RetryInvocation(gomock.Any(), gomock.Any()).DoAndReturn(
			func(ctx context.Context, req *pb.RetryInvocationRequest, opts ...grpc.CallOption) (*pb.RetryInvocationResponse, error) {
				retried <- req
				return &pb.RetryInvocationResponse{InvocationId: "retry-1"}, nil
			})
	})

	req := <-retried
	require.Equal(t, "func1", req.Invoke.HandlerName)
}

func TestPostDispatchHookPanicIsContained(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	reported := false
	executeHandlerWithAgent(t, controller, func(ctx HandlerContext) error { return fmt.Errorf("ohnoes") }, 0,
		func(*pb.ReportInvocationRequest) { reported = true },
		func(agent *Agent, mock *mockGrpcClient) {
			agent.postDispatch = func(context.Context, InvocationInfo, *Failure) PostAction { panic("hook bug") }
			expectEventStream(mock)
		})
	require.True(t, reported)
}

func TestPreDispatchHookRunsBeforeHandler(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	var order []string
	executeHandlerWithAgent(t, controller, func(ctx HandlerContext) error {
		order = append(order, "handler")
		return nil
	}, 0, nil, func(agent *Agent, mock *mockGrpcClient) {
		agent.preDispatch = func(_ context.Context, info InvocationInfo) {
			require.Equal(t, "func1", info.HandlerName)
			order = append(order, "pre")
		}
	})
	require.Equal(t, []string{"pre", "handler"}, order)
}

func TestFailureEventReachesPostDispatchHook(t *testing.T) {
	rec := newHookRecorder(PostActionRetry)
	retried := make(chan *pb.RetryInvocationRequest, 1)

	ev := &pb.DispatchEvent{
		Invoke: &pb.DispatchHandlerInvoke{
			InvocationId: "lost-1",
			HandlerName:  "func1",
			Reason:       pb.HandlerInvokeType_WEBHOOK,
			Args:         map[string]string{"body": "payload"},
			Attempt:      2,
		},
		Failure: &pb.InvocationFailure{
			Reason:         pb.FailureReason_NO_RESOURCES,
			Source:         pb.FailureSource_RUNTIME,
			Message:        "dispatch queue is full",
			CallerRejected: true,
		},
	}

	controller := gomock.NewController(t)
	defer controller.Finish()
	executeHandlerWithAgent(t, controller, func(ctx HandlerContext) error {
		// Hold the run open until the event has been handled.
		<-rec.seen
		return nil
	}, 0, nil, func(agent *Agent, mock *mockGrpcClient) {
		agent.postDispatch = rec.hook
		expectEventStream(mock, ev)
		mock.agentStub.EXPECT().RetryInvocation(gomock.Any(), gomock.Any()).DoAndReturn(
			func(ctx context.Context, req *pb.RetryInvocationRequest, opts ...grpc.CallOption) (*pb.RetryInvocationResponse, error) {
				retried <- req
				return &pb.RetryInvocationResponse{InvocationId: "retry-1"}, nil
			})
	})

	rec.mu.Lock()
	first := rec.calls[0]
	rec.mu.Unlock()
	require.Equal(t, "lost-1", first.info.InvocationID)
	require.Equal(t, "payload", first.info.Args["body"])
	require.Equal(t, 2, first.info.Attempt)
	require.Equal(t, pb.FailureReason_NO_RESOURCES, first.failure.Reason)
	require.True(t, first.failure.CallerRejected)
	require.Equal(t, "lost-1", (<-retried).Invoke.InvocationId)
}

func TestFailureEventsStopOnOlderAgent(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	executeHandlerWithAgent(t, controller, func(ctx HandlerContext) error {
		time.Sleep(50 * time.Millisecond)
		return nil
	}, 0, nil, func(agent *Agent, mock *mockGrpcClient) {
		agent.postDispatch = func(context.Context, InvocationInfo, *Failure) PostAction { return PostActionIgnore }
		// Times(1): an agent without the RPC must not be redialed in a loop.
		mock.agentStub.EXPECT().DispatchEvents(gomock.Any(), gomock.Any()).Times(1).Return(
			&eventStream{err: status.Error(codes.Unimplemented, "unknown method DispatchEvents")}, nil)
	})
}

func TestDispatchRequestAdvertisesFailureEvents(t *testing.T) {
	for _, withHook := range []bool{false, true} {
		t.Run(fmt.Sprint(withHook), func(t *testing.T) {
			controller := gomock.NewController(t)
			defer controller.Finish()

			var sent *mockBidiClient
			executeHandlerWithAgent(t, controller, func(ctx HandlerContext) error { return nil }, 0, nil,
				func(agent *Agent, mock *mockGrpcClient) {
					if withHook {
						agent.postDispatch = func(context.Context, InvocationInfo, *Failure) PostAction { return PostActionIgnore }
						expectEventStream(mock)
					}
					sent = mock.lastStream
				})
			require.Len(t, sent.sent, 1)
			require.Equal(t, withHook, sent.sent[0].FailureEvents)
		})
	}
}

func TestInvokeDeadline(t *testing.T) {
	now := time.Now()
	agentDeadline := func(d time.Duration) *timestamppb.Timestamp { return timestamppb.New(now.Add(d)) }

	// Older agent: only the timeout counts.
	d, capped, expired := invokeDeadline(&pb.DispatchHandlerInvoke{TimeoutMs: 1000}, now)
	require.Equal(t, now.Add(time.Second), d)
	require.False(t, capped)
	require.False(t, expired)

	// Plenty of time left: the handler's own timeout is tighter.
	d, capped, expired = invokeDeadline(&pb.DispatchHandlerInvoke{TimeoutMs: 1000, Deadline: agentDeadline(time.Minute)}, now)
	require.Equal(t, now.Add(time.Second), d)
	require.False(t, capped)
	require.False(t, expired)

	// It waited before arriving: the agent's deadline, less the margin, wins.
	d, capped, expired = invokeDeadline(&pb.DispatchHandlerInvoke{TimeoutMs: 10000, Deadline: agentDeadline(3 * time.Second)}, now)
	require.WithinDuration(t, now.Add(2*time.Second), d, time.Millisecond)
	require.True(t, capped)
	require.False(t, expired)

	// Too late to start: the agent has reported it already.
	_, _, expired = invokeDeadline(&pb.DispatchHandlerInvoke{TimeoutMs: 10000, Deadline: agentDeadline(500 * time.Millisecond)}, now)
	require.True(t, expired)
}
