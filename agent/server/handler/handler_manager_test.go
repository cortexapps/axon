package handler

import (
	"context"
	"sort"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/server/cron"
	"github.com/prometheus/client_golang/prometheus"
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
	require.Equal(t, 3, len(metrics))

	expected := []string{
		"axon_handler_invokes",
		"axon_handler_latency",
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
