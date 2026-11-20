package handler

import (
	"context"
	"fmt"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/server/cron"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// recordingNotifier collects the failures the manager hands on.
type recordingNotifier struct {
	events chan *pb.DispatchEvent
}

func newRecordingNotifier() *recordingNotifier {
	return &recordingNotifier{events: make(chan *pb.DispatchEvent, 100)}
}

func (r *recordingNotifier) Notify(invoke *pb.DispatchHandlerInvoke, failure *pb.InvocationFailure) {
	r.events <- &pb.DispatchEvent{Invoke: invoke, Failure: failure}
}

func (r *recordingNotifier) next(t *testing.T) *pb.DispatchEvent {
	select {
	case ev := <-r.events:
		return ev
	case <-time.After(5 * time.Second):
		require.FailNow(t, "expected a failure")
		return nil
	}
}

func (r *recordingNotifier) requireNone(t *testing.T) {
	select {
	case ev := <-r.events:
		require.FailNow(t, "unexpected failure", "%v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

func webhookOnlyOption() *pb.HandlerOption {
	return &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{
				Type: pb.HandlerInvokeType_WEBHOOK,
			},
		},
	}
}

func startNotifiedWebhookHandler(t *testing.T, timeout time.Duration) (Manager, *recordingNotifier, HandlerEntry) {
	logger, _ := zap.NewDevelopment()
	mgr := NewHandlerManager(logger, cron.New(), nil)
	notifier := newRecordingNotifier()
	mgr.SetFailureNotifier(notifier)
	_, err := mgr.RegisterHandler("1", "handler1", timeout, webhookOnlyOption())
	require.NoError(t, err)
	require.NoError(t, mgr.Start("1"))
	return mgr, notifier, mgr.ListHandlers()[0]
}

// dequeueWithin waits up to wait for src's next item.
func dequeueWithin[T any](src InvocationSource[T], wait time.Duration) (T, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return src.Dequeue(ctx, "1")
}

func requireNoInvoke(t *testing.T, mgr Manager) {
	h, err := dequeueWithin(mgr, 200*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded, "unexpected invocation %v", h)
}

func TestInvokeStateTransitions(t *testing.T) {
	_, _, entry := startNotifiedWebhookHandler(t, defaultTimeout)
	invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)

	require.Equal(t, InvokeQueued, invoke.State())
	require.True(t, invoke.SetState(InvokeDispatched))
	// It leaves the queue once: it can't then expire or be rejected.
	require.False(t, invoke.SetState(InvokeExpired))
	require.False(t, invoke.SetState(InvokeRejected))
	require.Equal(t, InvokeDispatched, invoke.State())
}

func TestQueueFullIsAFailure(t *testing.T) {
	mgr, notifier, entry := startNotifiedWebhookHandler(t, defaultTimeout)

	for i := 0; i < dispatchQueueDepth; i++ {
		require.NoError(t, mgr.Trigger(NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)))
	}

	rejected := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, map[string]string{"body": "payload"})
	require.ErrorIs(t, mgr.Trigger(rejected), ErrDispatchQueueFull)
	require.Equal(t, InvokeRejected, rejected.State())

	ev := notifier.next(t)
	require.Equal(t, rejected.Id, ev.Invoke.InvocationId)
	require.Equal(t, "payload", ev.Invoke.Args["body"])
	require.Equal(t, pb.FailureReason_NO_RESOURCES, ev.Failure.Reason)
	require.Equal(t, pb.FailureSource_RUNTIME, ev.Failure.Source)
	require.True(t, ev.Failure.CallerRejected)
}

func TestExpiredInQueueIsAFailure(t *testing.T) {
	mgr, notifier, entry := startNotifiedWebhookHandler(t, 50*time.Millisecond)

	invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)
	require.NoError(t, mgr.Trigger(invoke))

	// Nothing takes it, so it expires in the queue.
	ev := notifier.next(t)
	require.Equal(t, invoke.Id, ev.Invoke.InvocationId)
	require.Equal(t, pb.FailureReason_NO_RESOURCES, ev.Failure.Reason)
	require.False(t, ev.Failure.CallerRejected)

	// And it is never handed out afterwards.
	requireNoInvoke(t, mgr)
}

func TestDispatchedInvocationDoesNotExpire(t *testing.T) {
	mgr, notifier, entry := startNotifiedWebhookHandler(t, 50*time.Millisecond)

	invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)
	require.NoError(t, mgr.Trigger(invoke))
	h, err := dequeueWithin(mgr, time.Second)
	require.NoError(t, err)
	require.Equal(t, InvokeDispatched, h.State())

	// Once a client has it, only its report or the agent's timeout ends it.
	time.Sleep(200 * time.Millisecond)
	notifier.requireNone(t)
	select {
	case <-invoke.Done():
		require.Fail(t, "a dispatched invocation completed on its queue timeout")
	default:
	}
}

func TestReportedFailureIsAFailure(t *testing.T) {
	mgr, notifier, entry := startNotifiedWebhookHandler(t, defaultTimeout)

	invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)
	require.NoError(t, mgr.Trigger(invoke))
	_, err := dequeueWithin(mgr, time.Second)
	require.NoError(t, err)

	invoke.Complete("", NewFailureError(pb.FailureReason_HANDLER_ERROR, pb.FailureSource_APPLICATION, "boom"))
	ev := notifier.next(t)
	require.Equal(t, invoke.Id, ev.Invoke.InvocationId)
	require.Equal(t, pb.FailureReason_HANDLER_ERROR, ev.Failure.Reason)
}

func TestPlainErrorIsNotAFailure(t *testing.T) {
	mgr, notifier, entry := startNotifiedWebhookHandler(t, defaultTimeout)

	invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)
	require.NoError(t, mgr.Trigger(invoke))
	_, err := dequeueWithin(mgr, time.Second)
	require.NoError(t, err)

	// An older client reports errors without a failure; that is only recorded.
	invoke.Complete("", fmt.Errorf("invocation error: boom"))
	notifier.requireNone(t)
}
