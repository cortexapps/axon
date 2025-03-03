package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/cron"
	"github.com/cortexapps/axon/server/handler"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestGRPCServer_RegisterHandlerAndDispatch(t *testing.T) {

	port := 51000 + time.Now().Nanosecond()%1000

	historyPath := filepath.Join(t.TempDir(), "history")
	defer func() {
		_ = os.RemoveAll(historyPath)
	}()

	config := config.AgentConfig{
		GrpcPort:         port,
		CortexApiBaseUrl: "http://localhost",
		CortexApiToken:   "test-token",
		DryRun:           false,
		DequeueWaitTime:  1 * time.Second,
		HistoryPath:      historyPath,
	}

	logger, _ := zap.NewDevelopment()
	manager := handler.NewHandlerManager(logger, cron.New())

	agent := NewAxonAgent(logger, config, manager)
	defer agent.Close()

	go func() {
		if err := agent.Start(context.Background()); err != nil {
			panic(err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	messages := make([]*pb.DispatchMessage, 0, 5)

	client := NewTestAxonClient(t, int32(port), func(msg *pb.DispatchMessage) (bool, error) {

		require.NotNil(t, msg)
		switch msg.Type {
		case pb.DispatchMessageType_DISPATCH_MESSAGE_INVOKE:

			invoke := msg.GetInvoke()
			require.Equal(t, "handler123", invoke.HandlerName)
			messages = append(messages, msg)
			if len(messages) >= 2 {
				return true, nil
			}

		case pb.DispatchMessageType_DISPATCH_MESSAGE_WORK_COMPLETED:
			return true, nil
		}
		return false, nil
	})

	_, err := client.RegisterHandler(ctx, "handler123",
		&pb.HandlerOption{
			Option: &pb.HandlerOption_Invoke{
				Invoke: &pb.HandlerInvokeOption{
					Type:  pb.HandlerInvokeType_RUN_INTERVAL,
					Value: "1ms",
				},
			},
		},
	)

	require.NoError(t, err)
	err = client.Run(ctx)

	require.NoError(t, err)

	// validate the history got written to the historyPath
	historyFiles, err := os.ReadDir(historyPath)
	require.NoError(t, err)
	require.Len(t, historyFiles, 1)

	loggedReq := &pb.HandlerExecution{}
	historyFile := filepath.Join(historyPath, historyFiles[0].Name())
	contents, err := os.ReadFile(historyFile)
	require.NoError(t, err)

	json.Unmarshal(contents, loggedReq)

	require.Equal(t, client.dispatchId, loggedReq.DispatchId)
	require.Equal(t, "handler123", loggedReq.HandlerName)
	require.Equal(t, messages[0].GetInvoke().InvocationId, loggedReq.InvocationId)

	// get registered handlers
	handlers, err := client.ListHandlers(ctx)
	require.NoError(t, err)
	require.Len(t, handlers, 1)

	// get handler history
	history, err := client.GetHandlerHistory(ctx, handlers[0].Name)
	require.NoError(t, err)
	require.Len(t, history, 1)

}

func TestGRPCServer_ClientAutoClose(t *testing.T) {

	port := 51000 + time.Now().Nanosecond()%123

	historyPath := filepath.Join(t.TempDir(), "history")
	defer func() {
		_ = os.RemoveAll(historyPath)
	}()

	config := config.AgentConfig{
		GrpcPort:         port,
		CortexApiBaseUrl: "http://localhost",
		CortexApiToken:   "test-token",
		DryRun:           false,
		DequeueWaitTime:  100 * time.Millisecond,
		HistoryPath:      historyPath,
	}

	logger, _ := zap.NewDevelopment()
	manager := handler.NewHandlerManager(logger, cron.New())

	agent := NewAxonAgent(logger, config, manager)
	defer agent.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		if err := agent.Start(ctx); err != nil {
			panic(err)
		}
	}()

	messages := make([]*pb.DispatchMessage, 0, 5)
	seenClose := false

	client := NewTestAxonClient(t, int32(port), func(msg *pb.DispatchMessage) (bool, error) {

		require.NotNil(t, msg)
		switch msg.Type {
		case pb.DispatchMessageType_DISPATCH_MESSAGE_INVOKE:

			invoke := msg.GetInvoke()
			require.Equal(t, "handler123", invoke.HandlerName)
			messages = append(messages, msg)

		case pb.DispatchMessageType_DISPATCH_MESSAGE_WORK_COMPLETED:
			seenClose = true
			return true, nil
		}
		return false, nil
	})

	_, err := client.RegisterHandler(ctx, "handler123",
		&pb.HandlerOption{
			Option: &pb.HandlerOption_Invoke{
				Invoke: &pb.HandlerInvokeOption{
					Type: pb.HandlerInvokeType_RUN_NOW,
				},
			},
		},
	)

	require.NoError(t, err)

	err = client.Run(ctx)

	require.NoError(t, err)

	require.True(t, seenClose)
	require.Equal(t, 1, len(messages))

}
