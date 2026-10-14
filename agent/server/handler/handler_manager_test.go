package handler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/cron"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNewManager(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	mgr := NewHandlerManager(logger, cron, prometheus.NewRegistry())

	require.NotNil(t, mgr)
}

func FixtureHandlerOption() *pb.HandlerOption {
	return &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{
				Type:  pb.HandlerInvokeType_RUN_INTERVAL,
				Value: "1ms",
			},
		},
	}
}

func TestRegisterHandler(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	mgr := NewHandlerManager(logger, cron, nil)

	id, err := mgr.RegisterHandler("1", "handler1", defaultTimeout, FixtureHandlerOption())
	require.NoError(t, err)
	require.NotNil(t, id)

	// register the same handler again, should be idempotent
	id2, err2 := mgr.RegisterHandler("1", "handler1", defaultTimeout, FixtureHandlerOption())
	require.NoError(t, err2)
	require.Equal(t, id, id2)

	// register handler under a different dispatch ID is a different handler
	id3, err3 := mgr.RegisterHandler("2", "handler1", defaultTimeout, FixtureHandlerOption())
	require.NoError(t, err3)
	require.NotEqual(t, id, id3)

	handlers := mgr.ListHandlers()
	require.Len(t, handlers, 2)
}

func TestUnregisterHandler(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	mgr := NewHandlerManager(logger, cron, nil)

	id, err := mgr.RegisterHandler("1", "handler1", defaultTimeout, FixtureHandlerOption())
	require.NoError(t, err)
	require.NotEmpty(t, id)

	mgr.UnregisterHandler(id)
	handlers := mgr.ListHandlers()
	require.Len(t, handlers, 0)
}

func TestClearHandlers(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	mgr := NewHandlerManager(logger, cron, nil)

	_, err := mgr.RegisterHandler("1", "handler1", defaultTimeout, FixtureHandlerOption())
	require.NoError(t, err)

	mgr.ClearHandlers("1")

	handlers := mgr.ListHandlers()
	require.Len(t, handlers, 0)
}

func TestTriggerAndDequeue(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	registry := prometheus.NewRegistry()
	mgr := NewHandlerManager(logger, cron, registry)

	id, err := mgr.RegisterHandler("1", "handler1", defaultTimeout, FixtureHandlerOption())
	require.NoError(t, err)

	err = mgr.Start("1")
	require.NoError(t, err)

	entry := NewHandlerEntry(id, "1", "handler1", time.Duration(0))
	require.NoError(t, err)
	invoke := NewScheduledHandlerInvoke(entry, pb.HandlerInvokeType_RUN_NOW)
	err = mgr.Trigger(invoke)
	require.NoError(t, err)
	h, err := mgr.Dequeue(context.Background(), "1", 500*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, h)
	require.Equal(t, "handler1", h.GetEntry().Name())
	require.Equal(t, defaultTimeout, h.GetEntry().Timeout())
	invoke.Complete("ok", nil)

	time.Sleep(time.Millisecond * 10)
	metrics, err := registry.Gather()
	require.NoError(t, err)
	require.Equal(t, 4, len(metrics))

	expected := []string{
		"axon_handler_invokes",
		"axon_handler_latency",
		"axon_handler_liveness",
		"axon_handler_queue_depth",
	}
	sort.Strings(expected)
	seen := []string{}

	for _, metric := range metrics {
		seen = append(seen, metric.GetName())
	}

	sort.Strings(seen)
	require.Equal(t, expected, seen)

}

func TestTriggerAndDequeueNotStarted(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	mgr := NewHandlerManager(logger, cron, nil)

	id, err := mgr.RegisterHandler("1", "handler1", defaultTimeout, FixtureHandlerOption())
	require.NoError(t, err)

	entry := NewHandlerEntry(id, "1", "handler1", time.Duration(0))
	invoke := NewScheduledHandlerInvoke(entry, pb.HandlerInvokeType_RUN_NOW)

	err = mgr.Trigger(invoke)
	require.Error(t, err)

}

func TestTriggerAndDequeueCustomTimeout(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	mgr := NewHandlerManager(logger, cron, nil)

	timeoutMs := int32(1000)
	timeout := time.Duration(timeoutMs) * time.Millisecond

	id, err := mgr.RegisterHandler("1", "handler1", timeout, FixtureHandlerOption())

	require.NoError(t, err)

	err = mgr.Start("1")
	require.NoError(t, err)

	entry := NewHandlerEntry(id, "1", "handler1", timeout)
	invoke := NewScheduledHandlerInvoke(entry, pb.HandlerInvokeType_RUN_NOW)

	err = mgr.Trigger(invoke)
	require.NoError(t, err)
	h, err := mgr.Dequeue(context.Background(), "1", 500*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, h)
	require.Equal(t, "handler1", h.GetEntry().Name())
	require.Equal(t, timeout, h.GetEntry().Timeout())
}

func TestTriggerAndDequeueTimeout(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	mgr := NewHandlerManager(logger, cron, nil)

	_, err := mgr.RegisterHandler("1", "handler1", defaultTimeout, FixtureHandlerOption())
	require.NoError(t, err)

	h, err := mgr.Dequeue(context.Background(), "1", time.Millisecond)
	require.NoError(t, err)
	require.Nil(t, h)
}

func TestTriggerAndDequeueTimeoutContext(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	mgr := NewHandlerManager(logger, cron, nil)

	_, err := mgr.RegisterHandler("1", "handler1", defaultTimeout, FixtureHandlerOption())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	h, err := mgr.Dequeue(ctx, "1", time.Hour)
	require.NoError(t, err)
	require.Nil(t, h)
}

func TestTriggerRejectsWhenQueueIsFull(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	mgr := NewHandlerManager(logger, cron.New(), nil)

	// The fixture option triggers on an interval, which would put unrelated
	// invocations into the same queue while it fills below.
	webhookOnly := &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{
				Type: pb.HandlerInvokeType_WEBHOOK,
			},
		},
	}

	id, err := mgr.RegisterHandler("1", "handler1", defaultTimeout, webhookOnly)
	require.NoError(t, err)
	require.NoError(t, mgr.Start("1"))

	entry := mgr.ListHandlers()[0]
	require.Equal(t, id, entry.Id())

	// Nothing dequeues, so the queue fills and every later trigger must be
	// refused rather than block the caller.
	for i := 0; i < dispatchQueueDepth; i++ {
		require.NoError(t, mgr.Trigger(NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)))
	}

	rejected := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)

	done := make(chan error, 1)
	go func() { done <- mgr.Trigger(rejected) }()

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrDispatchQueueFull)
	case <-time.After(5 * time.Second):
		require.Fail(t, "Trigger blocked on a full queue instead of rejecting")
	}

	// A refused invocation is finished, not left dangling for its full timeout.
	select {
	case <-rejected.Done():
	case <-time.After(5 * time.Second):
		require.Fail(t, "refused invocation was never completed")
	}
	_, err = rejected.GetResult()
	require.Error(t, err)
}

// newLivenessManager returns a started manager with one webhook-only handler
// whose invocations expire after 20ms, and a liveness policy that trips on
// the first timeout.
func newLivenessManager(t *testing.T, registry *prometheus.Registry) (*handlerManager, HandlerEntry) {
	t.Helper()
	logger, _ := zap.NewDevelopment()
	mgr := NewHandlerManager(logger, cron.New(), registry,
		WithLivenessPolicy(LivenessPolicy{TimeoutThreshold: 1})).(*handlerManager)

	webhookOnly := &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{Type: pb.HandlerInvokeType_WEBHOOK},
		},
	}
	_, err := mgr.RegisterHandler("1", "handler1", 20*time.Millisecond, webhookOnly)
	require.NoError(t, err)
	require.NoError(t, mgr.Start("1"))
	return mgr, mgr.ListHandlers()[0]
}

// An invocation that expires waiting in the dispatch queue is an agent-side
// timeout: the SDK never read it.
func TestTriggerCountsQueueExpiryAsTimeout(t *testing.T) {
	registry := prometheus.NewRegistry()
	mgr, entry := newLivenessManager(t, registry)

	invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)
	require.NoError(t, mgr.Trigger(invoke))
	<-invoke.Done()

	require.Eventually(t, func() bool {
		return mgr.Liveness().Snapshot().ConsecutiveTimeouts == 1
	}, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, float64(1), testutil.ToFloat64(mgr.timeoutCounter.WithLabelValues("handler1")))
}

// The server's sent-time timer completes an invocation with
// ErrInvocationTimeout; that is the other agent-side timeout.
func TestTriggerCountsServerTimeoutSentinel(t *testing.T) {
	mgr, entry := newLivenessManager(t, nil)

	invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)
	require.NoError(t, mgr.Trigger(invoke))
	dequeued, err := mgr.Dequeue(context.Background(), "1", time.Second)
	require.NoError(t, err)
	require.NotNil(t, dequeued)
	require.NoError(t, dequeued.Complete("", fmt.Errorf("%w: handler did not report a result", ErrInvocationTimeout)))

	require.Eventually(t, func() bool {
		return mgr.Liveness().Snapshot().ConsecutiveTimeouts == 1
	}, 2*time.Second, 10*time.Millisecond)
}

// An error the SDK reported, or a rejection because the queue is full, says
// nothing about whether the SDK is draining; neither counts as a timeout.
func TestTriggerDoesNotCountReportedErrors(t *testing.T) {
	mgr, entry := newLivenessManager(t, nil)

	invoke := NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)
	require.NoError(t, mgr.Trigger(invoke))
	dequeued, err := mgr.Dequeue(context.Background(), "1", time.Second)
	require.NoError(t, err)
	require.NoError(t, dequeued.Complete("", errors.New("boom")))
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(mgr.invokeCounter.WithLabelValues("handler1", "error")) == 1
	}, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, 0, mgr.Liveness().Snapshot().ConsecutiveTimeouts)
	require.Equal(t, float64(0), testutil.ToFloat64(mgr.timeoutCounter.WithLabelValues("handler1")))

	// Fill the queue with long-lived invocations, then get one refused.
	_, err = mgr.RegisterHandler("2", "handler2", time.Hour, &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{Invoke: &pb.HandlerInvokeOption{Type: pb.HandlerInvokeType_WEBHOOK}},
	})
	require.NoError(t, err)
	require.NoError(t, mgr.Start("2"))
	var entry2 HandlerEntry
	for _, e := range mgr.ListHandlers() {
		if e.Name() == "handler2" {
			entry2 = e
		}
	}
	require.NotNil(t, entry2)
	for i := 0; i < dispatchQueueDepth; i++ {
		require.NoError(t, mgr.Trigger(NewHandlerInvoke(entry2, pb.HandlerInvokeType_WEBHOOK, nil)))
	}
	rejected := NewHandlerInvoke(entry2, pb.HandlerInvokeType_WEBHOOK, nil)
	require.ErrorIs(t, mgr.Trigger(rejected), ErrDispatchQueueFull)
	<-rejected.Done()
	require.Equal(t, 0, mgr.Liveness().Snapshot().ConsecutiveTimeouts)
}

func TestNewHandlerManagerFromConfig_AppliesLivenessPolicy(t *testing.T) {
	cfg := config.AgentConfig{LivenessTimeoutThreshold: 7, LivenessMinFailureAge: time.Minute}
	mgr := NewHandlerManagerFromConfig(cfg, zap.NewNop(), cron.New(), nil)
	require.Equal(t, LivenessPolicy{TimeoutThreshold: 7, MinFailureAge: time.Minute}, mgr.Liveness().Policy())
}
