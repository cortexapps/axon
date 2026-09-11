package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/cron"
	"github.com/cortexapps/axon/server/handler"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestHandleWebhook(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	manager := handler.NewHandlerManager(logger, cron, nil)

	option := &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{
				Type:  pb.HandlerInvokeType_WEBHOOK,
				Value: "my-webhook-id",
			},
		},
	}

	_, err := manager.RegisterHandler("1", "test", 1, option)
	require.NoError(t, err)

	err = manager.Start("1")
	require.NoError(t, err)

	webhookHandler := NewWebhookHandler(config.AgentConfig{}, logger, manager, nil)
	mux := mux.NewRouter()
	webhookHandler.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)

	// The handler should not have been invoked yet
	assert.Nil(t, manager.GetByTag("my-webhook-id").LastInvoked())

	/**
	Make a request to the webhook to see if things actually happen
	*/
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/webhook/my-webhook-id", strings.NewReader("payload"))
	assert.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	/**
	Ensure that the handler was invoked
	*/
	assert.NotNil(t, manager.GetByTag("my-webhook-id").LastInvoked())
	handlerInvocation, err := manager.Dequeue(context.Background(), "1", 500*time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, "test", handlerInvocation.GetEntry().Name())

	expected := map[string]string{
		"body":         "payload",
		"content-type": "application/json",
		"url":          "/webhook/my-webhook-id",
	}
	require.Equal(t, expected, handlerInvocation.ToDispatchInvoke().Args)
}

func TestHandleWebhookRejectsWhenQueueIsFull(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	manager := handler.NewHandlerManager(logger, cron.New(), nil)

	option := &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{
				Type:  pb.HandlerInvokeType_WEBHOOK,
				Value: "my-webhook-id",
			},
		},
	}

	_, err := manager.RegisterHandler("1", "test", time.Minute, option)
	require.NoError(t, err)
	require.NoError(t, manager.Start("1"))

	webhookHandler := NewWebhookHandler(config.AgentConfig{}, logger, manager, nil)
	router := mux.NewRouter()
	webhookHandler.RegisterRoutes(router)
	ts := httptest.NewServer(router)
	defer ts.Close()

	post := func() int {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/webhook/my-webhook-id", strings.NewReader("payload"))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// Nothing is dequeuing, so the queue fills up and every webhook after
	// that must be refused instead of accepted and silently dropped.
	for i := 0; i < 100; i++ {
		require.Equal(t, http.StatusOK, post())
	}

	done := make(chan int, 1)
	go func() { done <- post() }()

	select {
	case status := <-done:
		require.Equal(t, http.StatusServiceUnavailable, status)
	case <-time.After(5 * time.Second):
		require.Fail(t, "webhook request hung on a full queue instead of returning 503")
	}
}
