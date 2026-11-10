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

func startWebhookHandler(t *testing.T, timeout time.Duration) (Manager, HandlerEntry) {
	logger, _ := zap.NewDevelopment()
	mgr := NewHandlerManager(logger, cron.New(), nil)
	_, err := mgr.RegisterHandler("1", "handler1", timeout, webhookOnlyOption())
	require.NoError(t, err)
	require.NoError(t, mgr.Start("1"))
	return mgr, mgr.ListHandlers()[0]
}

func nextFailure(t *testing.T, mgr Manager) *pb.DispatchEvent {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ev, err := mgr.NextFailure(ctx, "1")
	require.NoError(t, err, "expected a failure event")
	return ev
}

func requireNoFailure(t *testing.T, mgr Manager) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	ev, err := mgr.NextFailure(ctx, "1")
	require.Error(t, err, "unexpected failure event: %v", ev)
}

func TestQueueFullEmitsNoResources(t *testing.T) {
	mgr, entry := startWebhookHandler(t, defaultTimeout)
	mgr.SubscribeFailures("1")

	for i := 0; i < dispatchQueueDepth; i++ {
		require.NoError(t, mgr.Trigger(NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)))
	}

	rejected := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, map[string]string{"body": "payload"})
	require.ErrorIs(t, mgr.Trigger(rejected), ErrDispatchQueueFull)

	ev := nextFailure(t, mgr)
	require.Equal(t, rejected.Id, ev.Invoke.InvocationId)
	require.Equal(t, "payload", ev.Invoke.Args["body"])
	require.Equal(t, pb.FailureReason_NO_RESOURCES, ev.Failure.Reason)
	require.Equal(t, pb.FailureSource_RUNTIME, ev.Failure.Source)
	require.True(t, ev.Failure.CallerRejected)
}

func TestExpiredInQueueEmitsNoResources(t *testing.T) {
	mgr, entry := startWebhookHandler(t, 50*time.Millisecond)
	mgr.SubscribeFailures("1")

	invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)
	require.NoError(t, mgr.Trigger(invoke))

	// Nothing dequeues, so it expires in the queue.
	ev := nextFailure(t, mgr)
	require.Equal(t, invoke.Id, ev.Invoke.InvocationId)
	require.Equal(t, pb.FailureReason_NO_RESOURCES, ev.Failure.Reason)
	require.False(t, ev.Failure.CallerRejected)

	// The dispatch loop must not send it once it has expired.
	h, err := mgr.Dequeue(context.Background(), "1", time.Second)
	require.NoError(t, err)
	require.NotNil(t, h)
	require.False(t, h.ClaimDispatch())
}

func TestDispatchedInvocationDoesNotEmitOnExpiry(t *testing.T) {
	mgr, entry := startWebhookHandler(t, 50*time.Millisecond)
	mgr.SubscribeFailures("1")

	require.NoError(t, mgr.Trigger(NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)))
	h, err := mgr.Dequeue(context.Background(), "1", time.Second)
	require.NoError(t, err)
	require.True(t, h.ClaimDispatch())

	// The agent's report timer owns timeouts for dispatched work.
	<-h.Done()
	requireNoFailure(t, mgr)
}

func TestFailuresWithoutSubscriberAreNotQueued(t *testing.T) {
	mgr, entry := startWebhookHandler(t, defaultTimeout)

	mgr.EmitFailure(NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil).ToDispatchInvoke(),
		&pb.InvocationFailure{Reason: pb.FailureReason_TIMEOUT})

	// Subscribing later must not surface the earlier event.
	requireNoFailure(t, mgr)
}

func TestFailuresAreNeverDropped(t *testing.T) {
	mgr, entry := startWebhookHandler(t, defaultTimeout)
	mgr.SubscribeFailures("1")

	const count = 10000
	for i := 0; i < count; i++ {
		invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, map[string]string{"n": fmt.Sprint(i)})
		mgr.EmitFailure(invoke.ToDispatchInvoke(), &pb.InvocationFailure{Reason: pb.FailureReason_TIMEOUT})
	}

	for i := 0; i < count; i++ {
		ev := nextFailure(t, mgr)
		require.Equal(t, fmt.Sprint(i), ev.Invoke.Args["n"], "events must arrive in order")
	}
	requireNoFailure(t, mgr)
}

func TestRequeuedFailureIsDeliveredNext(t *testing.T) {
	mgr, entry := startWebhookHandler(t, defaultTimeout)
	mgr.SubscribeFailures("1")

	for _, n := range []string{"a", "b"} {
		invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, map[string]string{"n": n})
		mgr.EmitFailure(invoke.ToDispatchInvoke(), &pb.InvocationFailure{Reason: pb.FailureReason_TIMEOUT})
	}

	first := nextFailure(t, mgr)
	require.Equal(t, "a", first.Invoke.Args["n"])
	mgr.RequeueFailure("1", first)

	require.Equal(t, "a", nextFailure(t, mgr).Invoke.Args["n"])
	require.Equal(t, "b", nextFailure(t, mgr).Invoke.Args["n"])
}

func TestRetriggerQueuesNextAttempt(t *testing.T) {
	mgr, entry := startWebhookHandler(t, defaultTimeout)

	original := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, map[string]string{"body": "payload"})
	id, err := mgr.Retrigger(original.ToDispatchInvoke())
	require.NoError(t, err)
	require.NotEqual(t, original.Id, id)

	h, err := mgr.Dequeue(context.Background(), "1", time.Second)
	require.NoError(t, err)
	retried := h.ToDispatchInvoke()
	require.Equal(t, id, retried.InvocationId)
	require.Equal(t, int32(2), retried.Attempt)
	require.Equal(t, "payload", retried.Args["body"])
	require.Equal(t, pb.HandlerInvokeType_WEBHOOK, retried.Reason)
}

func TestRetriggerFindsReregisteredHandler(t *testing.T) {
	mgr, entry := startWebhookHandler(t, defaultTimeout)

	stale := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil).ToDispatchInvoke()
	stale.HandlerId = "gone"

	_, err := mgr.Retrigger(stale)
	require.NoError(t, err)
}

func TestRetriggerStopsAtLimit(t *testing.T) {
	mgr, entry := startWebhookHandler(t, defaultTimeout)

	last := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil).ToDispatchInvoke()
	last.Attempt = maxRetryAttempts

	_, err := mgr.Retrigger(last)
	require.ErrorIs(t, err, ErrRetryLimit)
}

func TestRetriggerTreatsMissingAttemptAsFirst(t *testing.T) {
	mgr, entry := startWebhookHandler(t, defaultTimeout)

	fromOlderAgent := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil).ToDispatchInvoke()
	fromOlderAgent.Attempt = 0

	_, err := mgr.Retrigger(fromOlderAgent)
	require.NoError(t, err)
	h, err := mgr.Dequeue(context.Background(), "1", time.Second)
	require.NoError(t, err)
	require.Equal(t, int32(2), h.ToDispatchInvoke().Attempt)
}
