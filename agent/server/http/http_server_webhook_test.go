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
	ts := httptest.NewServer(webhookHandler)

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
	assert.Equal(t, http.StatusOK, resp.StatusCode)

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
