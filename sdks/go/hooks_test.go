package axon

import (
	"context"
	"fmt"
	"io"
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

// reportedFailure runs handler once and returns the failure it reported to
// the agent, or nil.
func reportedFailure(t *testing.T, handler any, timeoutMs int, setup ...func(*mockGrpcClient)) *pb.InvocationFailure {
	controller := gomock.NewController(t)
	defer controller.Finish()
	var reported *pb.ReportInvocationRequest
	executeHandlerWithAgent(t, controller, handler, timeoutMs,
		func(req *pb.ReportInvocationRequest) { reported = req },
		func(_ *Agent, mock *mockGrpcClient) {
			for _, s := range setup {
				s(mock)
			}
		})
	require.NotNil(t, reported)
	return reported.GetFailure()
}

func TestSuccessReportsNoFailure(t *testing.T) {
	require.Nil(t, reportedFailure(t, func(ctx HandlerContext) error { return nil }, 0))
}

func TestHandlerErrorIsReportedAsApplicationFailure(t *testing.T) {
	failure := reportedFailure(t, func(ctx HandlerContext) error { return fmt.Errorf("ohnoes") }, 0)
	require.NotNil(t, failure)
	require.Equal(t, pb.FailureReason_HANDLER_ERROR, failure.Reason)
	require.Equal(t, pb.FailureSource_APPLICATION, failure.Source)
	require.Equal(t, "ohnoes", failure.Message)
}

func TestPanicIsReportedAsHandlerError(t *testing.T) {
	failure := reportedFailure(t, func(ctx HandlerContext) error { panic("ohnoes") }, 0)
	require.Equal(t, pb.FailureReason_HANDLER_ERROR, failure.Reason)
	require.Equal(t, pb.FailureSource_APPLICATION, failure.Source)
}

func TestSlowHandlerIsReportedAsApplicationTimeout(t *testing.T) {
	failure := reportedFailure(t, func(ctx HandlerContext) error {
		time.Sleep(50 * time.Millisecond)
		return nil
	}, 1)
	require.Equal(t, pb.FailureReason_TIMEOUT, failure.Reason)
	require.Equal(t, pb.FailureSource_APPLICATION, failure.Source)
}

func TestTimeoutDuringApiCallIsReportedAsRuntime(t *testing.T) {
	failure := reportedFailure(t, func(ctx HandlerContext) error {
		_, err := ctx.CortexJsonApiCall("GET", "/api/v1/slow", "")
		return err
	}, 20, func(mock *mockGrpcClient) {
		mock.apiStub.EXPECT().Call(gomock.Any(), gomock.Any()).DoAndReturn(
			func(ctx context.Context, req *pb.CallRequest, opts ...grpc.CallOption) (*pb.CallResponse, error) {
				<-ctx.Done()
				return nil, status.FromContextError(ctx.Err()).Err()
			})
	})
	require.Equal(t, pb.FailureReason_TIMEOUT, failure.Reason)
	require.Equal(t, pb.FailureSource_RUNTIME, failure.Source)
}

func TestFailureHookNotRunLocally(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()
	called := false
	executeHandlerWithAgent(t, controller, func(ctx HandlerContext) error { return fmt.Errorf("ohnoes") }, 0, nil,
		func(agent *Agent, mock *mockGrpcClient) {
			// The agent sends the failure back as an event; it is not run here.
			agent.onFailure = func(context.Context, InvocationInfo, *Failure) FailureAction {
				called = true
				return FailureActionDrop
			}
			expectEventStream(mock)
		})
	require.False(t, called)
}

// eventStream is a DispatchEvents stream: it delivers events, records the
// client's messages, and then blocks until the client stops reading.
type eventStream struct {
	grpc.ClientStream
	ctx    context.Context
	events chan *pb.DispatchEvent
	err    error

	mu   sync.Mutex
	sent []*pb.DispatchEventsRequest
	// answered gets each resolution the client sends.
	answered chan *pb.FailureResolution
}

func (s *eventStream) Send(req *pb.DispatchEventsRequest) error {
	s.mu.Lock()
	s.sent = append(s.sent, req)
	s.mu.Unlock()
	if req.Resolution != nil {
		s.answered <- req.Resolution
	}
	return nil
}

func (s *eventStream) Recv() (*pb.DispatchEvent, error) {
	if s.err != nil {
		return nil, s.err
	}
	select {
	case ev := <-s.events:
		return ev, nil
	case <-s.ctx.Done():
		return nil, io.EOF
	}
}

func (s *eventStream) CloseSend() error { return nil }

// expectEventStream serves events on the first DispatchEvents stream the
// client opens, and returns the stream so the test can see its answers.
func expectEventStream(mock *mockGrpcClient, events ...*pb.DispatchEvent) *eventStream {
	stream := &eventStream{
		ctx:      context.Background(),
		events:   make(chan *pb.DispatchEvent, len(events)),
		answered: make(chan *pb.FailureResolution, len(events)+1),
	}
	for _, ev := range events {
		stream.events <- ev
	}
	mock.agentStub.EXPECT().DispatchEvents(gomock.Any(), gomock.Any()).AnyTimes().DoAndReturn(
		func(ctx context.Context, opts ...grpc.CallOption) (grpc.BidiStreamingClient[pb.DispatchEventsRequest, pb.DispatchEvent], error) {
			stream.ctx = ctx
			return stream, nil
		})
	return stream
}

func failureEvent(id string, attempt int32) *pb.DispatchEvent {
	return &pb.DispatchEvent{
		Invoke: &pb.DispatchHandlerInvoke{
			InvocationId: id,
			HandlerName:  "func1",
			Reason:       pb.HandlerInvokeType_WEBHOOK,
			Args:         map[string]string{"body": "payload-" + id},
			Attempt:      attempt,
		},
		Failure: &pb.InvocationFailure{
			Reason:         pb.FailureReason_NO_RESOURCES,
			Source:         pb.FailureSource_RUNTIME,
			Message:        "dispatch queue is full",
			CallerRejected: true,
		},
	}
}

// runWithEvents runs one handler invocation while the agent sends events.
// The handler holds the run open until every event has been answered.
func runWithEvents(t *testing.T, hook FailureHook, events ...*pb.DispatchEvent) *eventStream {
	controller := gomock.NewController(t)
	defer controller.Finish()
	var stream *eventStream
	answeredAll := make(chan struct{})
	executeHandlerWithAgent(t, controller, func(ctx HandlerContext) error {
		select {
		case <-answeredAll:
		case <-time.After(5 * time.Second):
		}
		return nil
	}, 0, nil, func(agent *Agent, mock *mockGrpcClient) {
		agent.onFailure = hook
		stream = expectEventStream(mock, events...)
		go func() {
			for {
				stream.mu.Lock()
				n := 0
				for _, req := range stream.sent {
					if req.Resolution != nil {
						n++
					}
				}
				stream.mu.Unlock()
				if n >= len(events) {
					close(answeredAll)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	})
	return stream
}

func TestFailureEventRunsHookAndAnswers(t *testing.T) {
	var got InvocationInfo
	var gotFailure *Failure
	stream := runWithEvents(t, func(_ context.Context, info InvocationInfo, failure *Failure) FailureAction {
		got, gotFailure = info, failure
		return FailureActionRetry
	}, failureEvent("lost-1", 2))

	require.Equal(t, "lost-1", got.InvocationID)
	require.Equal(t, "payload-lost-1", got.Args["body"])
	require.Equal(t, 2, got.Attempt)
	require.Equal(t, pb.FailureReason_NO_RESOURCES, gotFailure.Reason)
	require.True(t, gotFailure.CallerRejected)

	stream.mu.Lock()
	defer stream.mu.Unlock()
	require.Equal(t, "", stream.sent[0].GetResolution().GetInvocationId(), "the first message only identifies the client")
	require.NotEmpty(t, stream.sent[0].DispatchId)
	res := stream.sent[1].GetResolution()
	require.Equal(t, "lost-1", res.InvocationId)
	require.Equal(t, pb.FailureAction_FAILURE_ACTION_RETRY, res.Action)
}

func TestFailureHookDropIsAnswered(t *testing.T) {
	stream := runWithEvents(t, func(context.Context, InvocationInfo, *Failure) FailureAction {
		return FailureActionDrop
	}, failureEvent("lost-1", 1))
	require.Equal(t, pb.FailureAction_FAILURE_ACTION_DROP, (<-stream.answered).Action)
}

func TestFailureHookPanicDrops(t *testing.T) {
	stream := runWithEvents(t, func(context.Context, InvocationInfo, *Failure) FailureAction {
		panic("hook bug")
	}, failureEvent("lost-1", 1), failureEvent("lost-2", 1))
	// The panic is contained, and the stream keeps going.
	require.Equal(t, "lost-1", (<-stream.answered).InvocationId)
	second := <-stream.answered
	require.Equal(t, "lost-2", second.InvocationId)
	require.Equal(t, pb.FailureAction_FAILURE_ACTION_DROP, second.Action)
}

func TestFailureEventsStopOnOlderAgent(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	executeHandlerWithAgent(t, controller, func(ctx HandlerContext) error {
		time.Sleep(50 * time.Millisecond)
		return nil
	}, 0, nil, func(agent *Agent, mock *mockGrpcClient) {
		agent.onFailure = func(context.Context, InvocationInfo, *Failure) FailureAction { return FailureActionDrop }
		// Times(1): an agent without the RPC must not be redialed in a loop.
		mock.agentStub.EXPECT().DispatchEvents(gomock.Any(), gomock.Any()).Times(1).Return(
			&eventStream{err: status.Error(codes.Unimplemented, "unknown method DispatchEvents"), answered: make(chan *pb.FailureResolution, 1)}, nil)
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
						agent.onFailure = func(context.Context, InvocationInfo, *Failure) FailureAction { return FailureActionDrop }
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
