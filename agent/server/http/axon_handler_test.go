package http

import (
	"context"
	"fmt"
	"io"
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

func TestInvokeEndpoint(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	manager := handler.NewHandlerManager(logger, cron, nil)

	option := &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{
				Type: pb.HandlerInvokeType_INVOKE,
			},
		},
	}

	_, err := manager.RegisterHandler("1", "test-handler", 1, option)
	require.NoError(t, err)

	err = manager.Start("1")
	require.NoError(t, err)

	axonHandlerParams := AxonHandlerParams{
		Logger:         logger,
		Config:         config.AgentConfig{},
		HandlerManager: manager,
	}
	axonHandler := NewAxonHandler(axonHandlerParams)
	mux := mux.NewRouter()
	axonHandler.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)

	payload := `{"body":"payload"}`
	response := `{"status":"ok"}`

	go func() {
		handlerInvocation, err := manager.Dequeue(context.Background(), "1", 500*time.Millisecond)
		require.NoError(t, err)
		require.NotNil(t, handlerInvocation)
		require.Equal(t, "test-handler", handlerInvocation.GetEntry().Name())
		require.Equal(t, payload, handlerInvocation.ToDispatchInvoke().Args["body"])

		handlerInvocation.Complete(response, nil)
	}()

	/**
	Make a request to the webhook to see if things actually happen
	*/
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/__axon/handlers/test-handler/invoke", strings.NewReader(payload))
	assert.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.NotNil(t, manager.GetByTag("test-handler").LastInvoked())
	body, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	require.Equal(t, response, string(body))

}

func TestInvokeEndpointErr(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	manager := handler.NewHandlerManager(logger, cron, nil)

	option := &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{
				Type: pb.HandlerInvokeType_INVOKE,
			},
		},
	}

	_, err := manager.RegisterHandler("1", "test-handler", 1, option)
	require.NoError(t, err)

	err = manager.Start("1")
	require.NoError(t, err)

	axonHandlerParams := AxonHandlerParams{
		Logger:         logger,
		Config:         config.AgentConfig{},
		HandlerManager: manager,
	}
	axonHandler := NewAxonHandler(axonHandlerParams)
	mux := mux.NewRouter()
	axonHandler.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)

	payload := `{"body":"payload"}`

	go func() {
		handlerInvocation, err := manager.Dequeue(context.Background(), "1", 500*time.Millisecond)
		require.NoError(t, err)
		require.Equal(t, "test-handler", handlerInvocation.GetEntry().Name())
		require.Equal(t, payload, handlerInvocation.ToDispatchInvoke().Args["body"])

		handlerInvocation.Complete("", fmt.Errorf("nope didn't work"))
	}()

	/**
	Make a request to the webhook to see if things actually happen
	*/
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/__axon/handlers/test-handler/invoke", strings.NewReader(payload))
	assert.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	assert.NotNil(t, manager.GetByTag("test-handler").LastInvoked())
	body, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	require.Equal(t, "{\"error\":\"Handler failed: nope didn't work\"}", string(body))

}
