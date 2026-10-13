package server

import (
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/cron"
	"github.com/cortexapps/axon/server/handler"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// An invocation's expiry timer can fire after the agent has closed. It must
// finish the invocation quietly, not dereference torn-down state.
func TestExpireInvocation_AfterCloseDoesNotPanic(t *testing.T) {
	logger := zap.NewNop()
	manager := handler.NewHandlerManager(logger, cron.New(), nil)
	agent := NewAxonAgent(Params{Logger: logger, Config: config.AgentConfig{HandlerHistoryPath: t.TempDir()}, Manager: manager})

	_, err := manager.RegisterHandler("d", "handler1", time.Second)
	require.NoError(t, err)
	entry := manager.ListHandlers()[0]
	invoke := handler.NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)
	msg := invoke.ToDispatchInvoke()
	agent.setOutstandingRequest(msg.InvocationId, &inflightRequest{invocable: invoke, sentAt: time.Now()})

	agent.Close()

	require.NotPanics(t, func() { agent.expireInvocation(msg) })
	select {
	case <-invoke.Done():
	default:
		t.Fatal("expiry after close should still complete the invocation")
	}
}

// Close stops the expiry timers of invocations still in flight, so nothing
// keeps running against a closed agent.
func TestClose_StopsPendingExpiryTimers(t *testing.T) {
	logger := zap.NewNop()
	manager := handler.NewHandlerManager(logger, cron.New(), nil)
	agent := NewAxonAgent(Params{Logger: logger, Config: config.AgentConfig{HandlerHistoryPath: t.TempDir()}, Manager: manager})

	_, err := manager.RegisterHandler("d", "handler1", time.Second)
	require.NoError(t, err)
	entry := manager.ListHandlers()[0]
	invoke := handler.NewHandlerInvoke(entry, pb.HandlerInvokeType_WEBHOOK, nil)
	msg := invoke.ToDispatchInvoke()
	msg.TimeoutMs = 50
	agent.setOutstandingRequest(msg.InvocationId, &inflightRequest{invocable: invoke, sentAt: time.Now()})

	fired := make(chan struct{})
	go func() {
		defer close(fired)
		agent.waitForExpiry(msg)
	}()
	agent.Close()

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("expiry goroutine did not stop on Close")
	}
	select {
	case <-invoke.Done():
		t.Fatal("a stopped timer must not expire the invocation")
	default:
	}
}
