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

type clientCallback func(m *pb.DispatchMessage) (bool, error)

type testAxonClient struct {
	client     pb.AxonAgentClient
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

	//log wheterh c is finished or not

	logger.Info("is c finished ", zap.Bool("finished", c.finished))
	for !c.finished {
		m, err := stream.Recv()

		logger.Error("error from stream is ", zap.Error(err))
		if err != nil {
			return err
		}
		start := time.Now()

		logger.Info("Making the callback call")
		exit, err := c.callback(m)

		if err == io.EOF || exit {

			return nil

		}

		logger.Info("inside the !c finished loop")
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
			}

			_, err = c.client.ReportInvocation(ctx, report)
			require.NoError(c.t, err)
		}
	}
	return nil
}

func (c *testAxonClient) Close() {
	c.finished = true
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
