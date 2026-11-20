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

func webhookOnlyOption() *pb.HandlerOption {
	return &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{
				Type: pb.HandlerInvokeType_WEBHOOK,
			},
		},
	}
}

func startWebhookHandler(t *testing.T, timeout time.Duration) (Manager, FailureManager, HandlerEntry) {
	logger, _ := zap.NewDevelopment()
	mgr := NewHandlerManager(logger, cron.New(), nil)
	failures := NewFailureManager(logger, nil, mgr)
	_, err := mgr.RegisterHandler("1", "handler1", timeout, webhookOnlyOption())
	require.NoError(t, err)
	require.NoError(t, mgr.Start("1"))
	return mgr, failures, mgr.ListHandlers()[0]
}

// dequeueWithin waits up to wait for src's next item.
func dequeueWithin[T any](src InvocationSource[T], wait time.Duration) (T, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return src.Dequeue(ctx, "1")
}

func nextFailure(t *testing.T, failures FailureManager) *FailureEvent {
	ev, err := dequeueWithin(failures, 5*time.Second)
	require.NoError(t, err, "expected a failure event")
	return ev
}

func requireNoFailure(t *testing.T, failures FailureManager) {
	ev, err := dequeueWithin(failures, 200*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded, "unexpected failure event %v", ev)
}

func requireNoInvoke(t *testing.T, mgr Manager) {
	h, err := dequeueWithin(mgr, 200*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded, "unexpected invocation %v", h)
}

func TestInvokeStateTransitions(t *testing.T) {
	_, _, entry := startWebhookHandler(t, defaultTimeout)
	invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)

	require.Equal(t, InvokeQueued, invoke.State())
	require.True(t, invoke.SetState(InvokeDispatched))
	// It leaves the queue once: it can't then expire or be rejected.
	require.False(t, invoke.SetState(InvokeExpired))
	require.False(t, invoke.SetState(InvokeRejected))
	require.Equal(t, InvokeDispatched, invoke.State())
}

func TestQueueFullIsAFailure(t *testing.T) {
	mgr, failures, entry := startWebhookHandler(t, defaultTimeout)
	failures.Subscribe("1")

	for i := 0; i < dispatchQueueDepth; i++ {
		require.NoError(t, mgr.Trigger(NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)))
	}

	rejected := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, map[string]string{"body": "payload"})
	require.ErrorIs(t, mgr.Trigger(rejected), ErrDispatchQueueFull)
	require.Equal(t, InvokeRejected, rejected.State())

	ev := nextFailure(t, failures)
	require.Equal(t, rejected.Id, ev.Invoke.InvocationId)
	require.Equal(t, "payload", ev.Invoke.Args["body"])
	require.Equal(t, pb.FailureReason_NO_RESOURCES, ev.Failure.Reason)
	require.Equal(t, pb.FailureSource_RUNTIME, ev.Failure.Source)
	require.True(t, ev.Failure.CallerRejected)
}

func TestExpiredInQueueIsAFailure(t *testing.T) {
	mgr, failures, entry := startWebhookHandler(t, 50*time.Millisecond)
	failures.Subscribe("1")

	invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)
	require.NoError(t, mgr.Trigger(invoke))

	// Nothing takes it, so it expires in the queue.
	ev := nextFailure(t, failures)
	require.Equal(t, invoke.Id, ev.Invoke.InvocationId)
	require.Equal(t, pb.FailureReason_NO_RESOURCES, ev.Failure.Reason)
	require.False(t, ev.Failure.CallerRejected)

	// And it is never handed out afterwards.
	requireNoInvoke(t, mgr)
}

func TestDispatchedInvocationDoesNotExpire(t *testing.T) {
	mgr, failures, entry := startWebhookHandler(t, 50*time.Millisecond)
	failures.Subscribe("1")

	invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)
	require.NoError(t, mgr.Trigger(invoke))
	h, err := dequeueWithin(mgr, time.Second)
	require.NoError(t, err)
	require.Equal(t, InvokeDispatched, h.State())

	// Once a client has it, only its report or the agent's timeout ends it.
	time.Sleep(200 * time.Millisecond)
	requireNoFailure(t, failures)
	select {
	case <-invoke.Done():
		require.Fail(t, "a dispatched invocation completed on its queue timeout")
	default:
	}
}

func TestReportedFailureIsAFailure(t *testing.T) {
	mgr, failures, entry := startWebhookHandler(t, defaultTimeout)
	failures.Subscribe("1")

	invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)
	require.NoError(t, mgr.Trigger(invoke))
	_, err := dequeueWithin(mgr, time.Second)
	require.NoError(t, err)

	invoke.Complete("", NewFailureError(pb.FailureReason_HANDLER_ERROR, pb.FailureSource_APPLICATION, "boom"))
	ev := nextFailure(t, failures)
	require.Equal(t, invoke.Id, ev.Invoke.InvocationId)
	require.Equal(t, pb.FailureReason_HANDLER_ERROR, ev.Failure.Reason)
}

func TestPlainErrorIsNotAFailure(t *testing.T) {
	mgr, failures, entry := startWebhookHandler(t, defaultTimeout)
	failures.Subscribe("1")

	invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)
	require.NoError(t, mgr.Trigger(invoke))
	_, err := dequeueWithin(mgr, time.Second)
	require.NoError(t, err)

	// An older client reports errors without a failure; that is only recorded.
	invoke.Complete("", fmt.Errorf("invocation error: boom"))
	requireNoFailure(t, failures)
}

func TestFailuresWithoutSubscriberAreNotQueued(t *testing.T) {
	_, failures, entry := startWebhookHandler(t, defaultTimeout)

	failures.Notify(NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil).ToDispatchInvoke(),
		&pb.InvocationFailure{Reason: pb.FailureReason_TIMEOUT})

	// Subscribing later must not surface the earlier event.
	requireNoFailure(t, failures)
}

func TestFailuresAreNeverDropped(t *testing.T) {
	_, failures, entry := startWebhookHandler(t, defaultTimeout)
	failures.Subscribe("1")

	const count = 10000
	for i := 0; i < count; i++ {
		invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, map[string]string{"n": fmt.Sprint(i)})
		failures.Notify(invoke.ToDispatchInvoke(), &pb.InvocationFailure{Reason: pb.FailureReason_TIMEOUT})
	}

	for i := 0; i < count; i++ {
		ev := nextFailure(t, failures)
		require.Equal(t, fmt.Sprint(i), ev.Invoke.Args["n"], "events must arrive in order")
	}
	requireNoFailure(t, failures)
}

func TestUnresolvedFailureIsRequeuedFirst(t *testing.T) {
	_, failures, entry := startWebhookHandler(t, defaultTimeout)
	failures.Subscribe("1")

	for _, n := range []string{"a", "b"} {
		invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, map[string]string{"n": n})
		failures.Notify(invoke.ToDispatchInvoke(), &pb.InvocationFailure{Reason: pb.FailureReason_TIMEOUT})
	}

	first := nextFailure(t, failures)
	require.Equal(t, "a", first.Invoke.Args["n"])
	first.Requeue()

	require.Equal(t, "a", nextFailure(t, failures).Invoke.Args["n"])
	require.Equal(t, "b", nextFailure(t, failures).Invoke.Args["n"])
}

func notifyFailure(failures FailureManager, invoke *pb.DispatchHandlerInvoke) {
	failures.Notify(invoke, &pb.InvocationFailure{Reason: pb.FailureReason_HANDLER_ERROR})
}

func TestResolveRetryQueuesNextAttempt(t *testing.T) {
	mgr, failures, entry := startWebhookHandler(t, defaultTimeout)
	failures.Subscribe("1")

	original := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, map[string]string{"body": "payload"})
	notifyFailure(failures, original.ToDispatchInvoke())
	require.NoError(t, nextFailure(t, failures).Resolve(pb.FailureAction_FAILURE_ACTION_RETRY))

	h, err := dequeueWithin(mgr, time.Second)
	require.NoError(t, err)
	retried := h.ToDispatchInvoke()
	require.NotEqual(t, original.Id, retried.InvocationId)
	require.Equal(t, int32(2), retried.Attempt)
	require.Equal(t, "payload", retried.Args["body"])
	require.Equal(t, pb.HandlerInvokeType_WEBHOOK, retried.Reason)
}

func TestResolveDropQueuesNothing(t *testing.T) {
	mgr, failures, entry := startWebhookHandler(t, defaultTimeout)
	failures.Subscribe("1")

	notifyFailure(failures, NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil).ToDispatchInvoke())
	ev := nextFailure(t, failures)
	require.NoError(t, ev.Resolve(pb.FailureAction_FAILURE_ACTION_DROP))

	requireNoInvoke(t, mgr)
	// Resolved once; a second answer is refused.
	require.Error(t, ev.Resolve(pb.FailureAction_FAILURE_ACTION_RETRY))
}

func TestRetryFindsReregisteredHandler(t *testing.T) {
	_, failures, entry := startWebhookHandler(t, defaultTimeout)
	failures.Subscribe("1")

	stale := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil).ToDispatchInvoke()
	stale.HandlerId = "gone"
	notifyFailure(failures, stale)

	require.NoError(t, nextFailure(t, failures).Resolve(pb.FailureAction_FAILURE_ACTION_RETRY))
}

func TestRetryStopsAtLimit(t *testing.T) {
	_, failures, entry := startWebhookHandler(t, defaultTimeout)
	failures.Subscribe("1")

	last := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil).ToDispatchInvoke()
	last.Attempt = maxRetryAttempts
	notifyFailure(failures, last)

	require.ErrorIs(t, nextFailure(t, failures).Resolve(pb.FailureAction_FAILURE_ACTION_RETRY), ErrRetryLimit)
}

func TestRetryTreatsMissingAttemptAsFirst(t *testing.T) {
	mgr, failures, entry := startWebhookHandler(t, defaultTimeout)
	failures.Subscribe("1")

	fromOlderAgent := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil).ToDispatchInvoke()
	fromOlderAgent.Attempt = 0
	notifyFailure(failures, fromOlderAgent)

	require.NoError(t, nextFailure(t, failures).Resolve(pb.FailureAction_FAILURE_ACTION_RETRY))
	h, err := dequeueWithin(mgr, time.Second)
	require.NoError(t, err)
	require.Equal(t, int32(2), h.ToDispatchInvoke().Attempt)
}

func TestConcurrentWaitersEachGetDistinctFailures(t *testing.T) {
	_, failures, entry := startWebhookHandler(t, defaultTimeout)
	failures.Subscribe("1")

	const waiters, count = 4, 200
	got := make(chan string, count)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < waiters; i++ {
		go func() {
			for {
				ev, err := failures.Dequeue(ctx, "1")
				if err != nil {
					return
				}
				got <- ev.Invoke.InvocationId
			}
		}()
	}

	for i := 0; i < count; i++ {
		notifyFailure(failures, NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil).ToDispatchInvoke())
	}

	seen := map[string]bool{}
	for i := 0; i < count; i++ {
		select {
		case id := <-got:
			require.False(t, seen[id], "failure %s handed out twice", id)
			seen[id] = true
		case <-time.After(5 * time.Second):
			require.FailNow(t, "waiters stalled", "%d of %d delivered", len(seen), count)
		}
	}
}

func TestFailureEventIsHandledOnce(t *testing.T) {
	_, failures, entry := startWebhookHandler(t, defaultTimeout)
	failures.Subscribe("1")

	notifyFailure(failures, NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil).ToDispatchInvoke())
	ev := nextFailure(t, failures)
	require.NoError(t, ev.Resolve(pb.FailureAction_FAILURE_ACTION_DROP))

	// A requeue after the answer, as when the stream ends, must not resend it.
	ev.Requeue()
	requireNoFailure(t, failures)
}
