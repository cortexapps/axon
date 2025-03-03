package handler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/proto"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestGetHistory(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	historyPath := t.TempDir()

	cfg := config.AgentConfig{
		HistoryPath: historyPath,
	}

	hm := NewHistoryManager(cfg, logger)

	// Create mock history files
	handlerName := "testHandler"
	timestamp := time.Now()
	req := &pb.ReportInvocationRequest{
		HandlerInvoke: &pb.DispatchHandlerInvoke{
			DispatchId:   "dispatch1",
			HandlerName:  handlerName,
			InvocationId: "invocation1",
		},
		StartClientTimestamp: timestamppb.New(timestamp),
		DurationMs:           100,
	}

	e := proto.ReportToExecution(req, time.Now())
	err := hm.Write(context.Background(), e)
	require.NoError(t, err)

	// Test GetHistory
	history, err := hm.GetHistory(context.Background(), handlerName, false, 0)
	require.NoError(t, err)
	require.Len(t, history, 1)

	execution := history[0]
	require.Equal(t, req.HandlerInvoke.DispatchId, execution.DispatchId)
	require.Equal(t, req.HandlerInvoke.HandlerName, execution.HandlerName)
	require.Equal(t, req.HandlerInvoke.InvocationId, execution.InvocationId)
	require.Equal(t, req.StartClientTimestamp, execution.StartClientTimestamp)
	require.Equal(t, req.DurationMs, execution.DurationMs)

	require.Equal(t, req.GetError(), execution.Error)
	require.Equal(t, req.Logs, execution.Logs)
}

func TestGetHistory_NoHistory(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	historyPath := t.TempDir()

	cfg := config.AgentConfig{
		HistoryPath: historyPath,
	}

	hm := NewHistoryManager(cfg, logger)

	// Test GetHistory with no history files
	history, err := hm.GetHistory(context.Background(), "nonExistentHandler", false, 0)
	require.NoError(t, err)
	require.Len(t, history, 0)
}

func TestGetHistory_NoDir(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	cfg := config.AgentConfig{
		HistoryPath: "/foo/bar",
	}

	hm := NewHistoryManager(cfg, logger)

	// Test GetHistory with no history files
	history, err := hm.GetHistory(context.Background(), "nonExistentHandler", false, 0)
	require.NoError(t, err)
	require.Len(t, history, 0)
}

func TestGetHistory_InvalidFiles(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	historyPath := t.TempDir()

	cfg := config.AgentConfig{
		HistoryPath: historyPath,
	}

	hm := NewHistoryManager(cfg, logger)

	// Create invalid history files
	invalidFilePath := filepath.Join(historyPath, "invalid-file.json")
	err := os.WriteFile(invalidFilePath, []byte("invalid content"), 0644)
	require.NoError(t, err)

	// Test GetHistory with invalid files
	history, err := hm.GetHistory(context.Background(), "testHandler", false, 0)
	require.NoError(t, err)
	require.Len(t, history, 0)
}

func TestGetHistory_MultipleFiles(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	historyPath := t.TempDir()

	cfg := config.AgentConfig{
		HistoryPath: historyPath,
	}

	hm := NewHistoryManager(cfg, logger)

	// Create multiple history files
	handlerName := "testHandler"
	timestamp1 := time.Now().Add(-time.Hour)
	timestamp2 := time.Now()

	req1 := &pb.ReportInvocationRequest{
		HandlerInvoke: &pb.DispatchHandlerInvoke{
			DispatchId:   "dispatch1",
			HandlerName:  handlerName,
			InvocationId: "invocation1",
		},
		StartClientTimestamp: timestamppb.New(timestamp1),
		DurationMs:           100,
		Logs: []*pb.Log{
			{Level: "INFO",
				Timestamp: timestamppb.New(time.Now()),
				Message:   "1-log1",
			},
			{Level: "INFO",
				Timestamp: timestamppb.New(time.Now()),
				Message:   "1-log2",
			},
		},
	}

	req2 := &pb.ReportInvocationRequest{
		HandlerInvoke: &pb.DispatchHandlerInvoke{
			DispatchId:   "dispatch2",
			HandlerName:  handlerName,
			InvocationId: "invocation2",
		},
		StartClientTimestamp: timestamppb.New(timestamp2),
		DurationMs:           200,
		Logs: []*pb.Log{
			{Level: "ERROR",
				Timestamp: timestamppb.New(time.Now()),
				Message:   "2-log1",
			},
		},
	}
	e1 := proto.ReportToExecution(req1, time.Now())

	err := hm.Write(context.Background(), e1)
	require.NoError(t, err)

	e2 := proto.ReportToExecution(req2, time.Now().Add(time.Second))
	err = hm.Write(context.Background(), e2)
	require.NoError(t, err)

	// Test GetHistory with multiple files
	history, err := hm.GetHistory(context.Background(), handlerName, true, 0)
	require.NoError(t, err)
	require.Len(t, history, 2)

	execution1 := history[0]
	require.Equal(t, req1.HandlerInvoke.DispatchId, execution1.DispatchId)
	require.Equal(t, req1.HandlerInvoke.HandlerName, execution1.HandlerName)
	require.Equal(t, req1.HandlerInvoke.InvocationId, execution1.InvocationId)
	require.Equal(t, req1.StartClientTimestamp, execution1.StartClientTimestamp)
	require.Equal(t, req1.DurationMs, execution1.DurationMs)
	require.Equal(t, req1.GetError(), execution1.Error)
	require.Equal(t, req1.Logs, execution1.Logs)

	execution2 := history[1]
	require.Equal(t, req2.HandlerInvoke.DispatchId, execution2.DispatchId)
	require.Equal(t, req2.HandlerInvoke.HandlerName, execution2.HandlerName)
	require.Equal(t, req2.HandlerInvoke.InvocationId, execution2.InvocationId)
	require.Equal(t, req2.StartClientTimestamp, execution2.StartClientTimestamp)
	require.Equal(t, req2.DurationMs, execution2.DurationMs)
	require.Equal(t, req2.GetError(), execution2.Error)
	require.Equal(t, req2.Logs, execution2.Logs)
}
