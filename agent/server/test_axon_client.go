package server

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type clientCallback func(m *pb.DispatchMessage) (clientResult, error)

type clientResult struct {
	done   bool
	err    error
	result string
}

type testAxonClient struct {
	client     pb.AxonAgentClient
	conn       *grpc.ClientConn
	dispatchId string
	callback   clientCallback
	finished   bool
	t          *testing.T
}

func NewTestAxonClient(t *testing.T, port int32, callback clientCallback) *testAxonClient {

	conn, err := grpc.NewClient(fmt.Sprintf("localhost:%d", port), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	client := pb.NewAxonAgentClient(conn)

	return &testAxonClient{
		client:     client,
		conn:       conn,
		dispatchId: fmt.Sprintf("%v", time.Now().UnixNano()),
		callback:   callback,
		t:          t,
	}
}

func (c *testAxonClient) RegisterHandler(ctx context.Context,
	name string,
	options ...*pb.HandlerOption,
) (string, error) {
	req := &pb.RegisterHandlerRequest{
		DispatchId:  c.dispatchId,
		HandlerName: name,
		Options:     options,
	}
	resp, err := c.client.RegisterHandler(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Id, nil
}

func (c *testAxonClient) UnregisterHandler(ctx context.Context, id string) error {
	_, err := c.client.UnregisterHandler(ctx, &pb.UnregisterHandlerRequest{Id: id})
	return err
}

func (c *testAxonClient) Run(ctx context.Context) error {
	logger, _ := zap.NewDevelopment()
	stream, err := c.client.Dispatch(ctx)
	if err != nil {
		return err
	}

	dispatchReq := &pb.DispatchRequest{
		DispatchId: c.dispatchId,
	}

	logger.Info("sending dispatch request")
	err = stream.Send(dispatchReq)
	if err != nil {
		logger.Error("ERIROIROROROR ERROR ROROROR")
		return err
	}

	for !c.finished {
		m, err := stream.Recv()

		if err != nil {
			logger.Error("error from stream is ", zap.Error(err))
			return err
		}
		start := time.Now()

		result, err := c.callback(m)

		if err == io.EOF || result.done {
			return nil
		}

		end := time.Now()
		invoke := m.GetInvoke()
		if invoke != nil {

			report := &pb.ReportInvocationRequest{
				HandlerInvoke:        invoke,
				StartClientTimestamp: timestamppb.New(start),
				DurationMs:           int32(end.Sub(start).Milliseconds()),
			}
			if err != nil {
				report.Message = &pb.ReportInvocationRequest_Error{
					Error: &pb.Error{
						Code:    "boom",
						Message: err.Error(),
					},
				}
			} else {
				report.Message = &pb.ReportInvocationRequest_Result{
					Result: &pb.InvokeResult{
						Value: result.result,
					},
				}
			}

			_, err = c.client.ReportInvocation(ctx, report)
			require.NoError(c.t, err)
		}
	}
	return nil
}

func (c *testAxonClient) Close() {
	c.finished = true
	c.conn.Close()
}

func (c *testAxonClient) ListHandlers(ctx context.Context) ([]*pb.HandlerInfo, error) {
	resp, err := c.client.ListHandlers(ctx, &pb.ListHandlersRequest{})
	if err != nil {
		return nil, err
	}
	return resp.Handlers, nil
}

func (c *testAxonClient) GetHandlerHistory(ctx context.Context, name string) ([]*pb.HandlerExecution, error) {
	resp, err := c.client.GetHandlerHistory(ctx, &pb.GetHandlerHistoryRequest{HandlerName: name})
	if err != nil {
		return nil, err
	}
	return resp.History, nil
}
