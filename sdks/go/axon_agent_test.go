package axon

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	pb "github.com/cortexapps/axon-go/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon-go/mock_axon"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc"
)

func TestRegisterUnregisterHandler(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()
	agent, mock := createAgent(controller)

	testHandler := func(ctx HandlerContext) error {
		return nil
	}

	id := "12345"
	timeout := 123 * time.Millisecond
	mock.agentStub.EXPECT().RegisterHandler(gomock.Any(), gomock.Any()).Return(&pb.RegisterHandlerResponse{Id: id}, nil)

	h, err := agent.RegisterHandler(testHandler,
		WithTimeout(timeout),
		WithInvokeOption(pb.HandlerInvokeType_RUN_INTERVAL, "10s"),
		WithInvokeOption(pb.HandlerInvokeType_RUN_NOW, ""),
		WithInvokeOption(pb.HandlerInvokeType_CRON_SCHEDULE, "1 2 3 4 5"),
	)
	require.NoError(t, err)
	require.NotEmpty(t, h)

	// Verify that the handler is registered correctly
	handlerInfo, ok := agent.registeredHandlers[h]
	require.True(t, ok)
	require.NotNil(t, handlerInfo)
	require.Equal(t, "func1", handlerInfo.name)
	require.Equal(t, timeout, handlerInfo.timeout)
	require.Len(t, handlerInfo.options, 3)

	for _, option := range handlerInfo.options {
		invoke := option.Option.(*pb.HandlerOption_Invoke).Invoke
		switch invoke.Type {
		case pb.HandlerInvokeType_RUN_NOW:
			require.Empty(t, invoke.Value)
		case pb.HandlerInvokeType_RUN_INTERVAL:
			require.Equal(t, "10s", invoke.Value)
		case pb.HandlerInvokeType_CRON_SCHEDULE:
			require.Equal(t, "1 2 3 4 5", invoke.Value)
		default:
			require.Fail(t, "unexpected option type")
		}
	}

	// Verify unregistering the handler
	mock.agentStub.EXPECT().UnregisterHandler(gomock.Any(), &pb.UnregisterHandlerRequest{Id: id}).Return(&pb.UnregisterHandlerResponse{}, nil)
	err = agent.UnregisterHandler(h)
	require.NoError(t, err)
	require.Empty(t, agent.registeredHandlers)
}

func TestInvokeHandler(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	called := false

	theHandler := func(ctx HandlerContext) error {
		called = true
		return nil
	}

	executeHandlerHelper(t, controller, theHandler, 0, nil)

	require.True(t, called)

}

func TestInvokeHandlerWithTimeout(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	called := false

	theHandler := func(ctx HandlerContext) error {
		called = true
		return nil
	}

	executeHandlerHelper(t, controller, theHandler, 1000, nil)

	require.True(t, called)

}

func TestInvokeHandlerWithTimeoutTriggered(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	called := false

	theHandler := func(ctx HandlerContext) error {
		called = true
		time.Sleep(time.Millisecond * 10)
		return nil
	}

	executeHandlerHelper(t, controller, theHandler, 1, func(req *pb.ReportInvocationRequest) {
		reportedErr := req.GetError()
		require.NotNil(t, reportedErr)
		require.Equal(t, "timeout", reportedErr.Code)
	})

	require.True(t, called)

}

func TestInvokeHandlerWithPanic(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	called := false

	theHandler := func(ctx HandlerContext) error {
		called = true
		panic("ohnoes")
	}

	executeHandlerHelper(t, controller, theHandler, 1, func(req *pb.ReportInvocationRequest) {
		reportedErr := req.GetError()
		require.NotNil(t, reportedErr)
		require.Equal(t, "unexpected", reportedErr.Code)
	})

	require.True(t, called)

}

func TestInvokeHandlerWithError(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	called := false

	theHandler := func(ctx HandlerContext) error {
		called = true
		return fmt.Errorf("ohnoes")
	}

	executeHandlerHelper(t, controller, theHandler, 1, func(req *pb.ReportInvocationRequest) {
		reportedErr := req.GetError()
		require.NotNil(t, reportedErr)
		require.Equal(t, "ohnoes", reportedErr.Message)
		require.Equal(t, "unexpected", reportedErr.Code)
	})

	require.True(t, called)

}

func TestInvokeHandlerApiCall(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	called := false

	theHandler := func(ctx HandlerContext) (any, error) {
		called = true
		resp, err := ctx.CortexJsonApiCall("GET", "/api/v1/test", "")
		require.NoError(t, err)
		return fmt.Sprintf("%v", resp.StatusCode), nil
	}

	executeHandlerHelper(t, controller, theHandler, 1, func(req *pb.ReportInvocationRequest) {
		reportedErr := req.GetError()
		require.Nil(t, reportedErr)
		require.Equal(t, "201", req.GetResult().Value)
	}, func(mock *mockGrpcClient) {

		req :=
			&pb.CallRequest{
				Method:      "GET",
				Path:        "/api/v1/test",
				Body:        "",
				ContentType: "application/json",
			}

		mock.apiStub.EXPECT().Call(gomock.Any(), gomock.Eq(req)).Return(&pb.CallResponse{StatusCode: 201}, nil)
	})

	require.True(t, called)

}

func TestInvokeHandlerApiCallWithError(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	called := false

	theHandler := func(ctx HandlerContext) (any, error) {
		called = true
		return nil, fmt.Errorf("ohnoes")
	}

	executeHandlerHelper(t, controller, theHandler, 1, func(req *pb.ReportInvocationRequest) {
		reportedErr := req.GetError()
		require.NotNil(t, reportedErr)
		require.Nil(t, req.GetResult())
	}, func(mock *mockGrpcClient) {

	})

	require.True(t, called)

}

//
// Helpers
//

func createAgent(controller *gomock.Controller) (*Agent, *mockGrpcClient) {
	agent := NewAxonAgent(WithSleepOnError(0))
	agent.client = &mockGrpcClient{
		apiStub:   mock_axon.NewMockCortexApiClient(controller),
		agentStub: mock_axon.NewMockAxonAgentClient(controller),
	}
	return agent, agent.client.(*mockGrpcClient)
}

type agentCallback func(*mockGrpcClient)

func executeHandlerHelper(t *testing.T, controller *gomock.Controller, handler any, timeoutMs int, reportCallback func(*pb.ReportInvocationRequest), callback ...agentCallback) error {

	agent, mock := createAgent(controller)
	id := fmt.Sprintf("%d", time.Now().UnixNano())

	stream := newBidiClient(&pb.DispatchMessage{
		Type: pb.DispatchMessageType_DISPATCH_MESSAGE_INVOKE,
		Message: &pb.DispatchMessage_Invoke{
			Invoke: &pb.DispatchHandlerInvoke{
				HandlerId:   id,
				HandlerName: "func1",
				Reason:      pb.HandlerInvokeType_RUN_NOW,
				TimeoutMs:   int32(timeoutMs),
			},
		},
	},
		&pb.DispatchMessage{
			Type: pb.DispatchMessageType_DISPATCH_MESSAGE_WORK_COMPLETED,
		},
	)

	mock.agentStub.EXPECT().RegisterHandler(gomock.Any(), gomock.Any()).Return(&pb.RegisterHandlerResponse{Id: id}, nil)
	mock.agentStub.EXPECT().Dispatch(gomock.Any(), gomock.Any()).AnyTimes().Return(stream, nil)
	mock.agentStub.EXPECT().ReportInvocation(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, req *pb.ReportInvocationRequest, opts ...grpc.CallOption) (*pb.ReportInvocationResponse, error) {
			if reportCallback != nil {
				reportCallback(req)
			}
			return &pb.ReportInvocationResponse{}, nil
		})

	if len(callback) > 0 {
		callback[0](mock)
	}

	options := []RegisterHandlerOption{
		WithInvokeOption(pb.HandlerInvokeType_RUN_NOW, ""),
	}

	if timeoutMs > 0 {
		options = append(options, WithTimeout(time.Duration(timeoutMs)*time.Millisecond))
	}

	var h string
	var err error

	if hFunc, ok := handler.(Handler); ok {
		h, err = agent.RegisterHandler(hFunc, options...)
	} else if ihFunc, ok := handler.(InvocableHandler); ok {
		h, err = agent.RegisterInvocableHandler(ihFunc, options...)
	} else {
		panic("unexpected handler type: " + fmt.Sprintf("%T", handler))
	}

	require.NoError(t, err)
	require.NotEmpty(t, h)

	err = agent.Run(context.Background())

	require.NoError(t, err)
	return nil
}

type mockGrpcClient struct {
	apiStub   *mock_axon.MockCortexApiClient
	agentStub *mock_axon.MockAxonAgentClient
}

func (m *mockGrpcClient) api() pb.CortexApiClient {
	return m.apiStub
}

func (m *mockGrpcClient) agent() pb.AxonAgentClient {
	return m.agentStub
}

type mockBidiClient struct {
	grpc.ClientStream

	messages     []*pb.DispatchMessage
	messageIndex int
	eofOnFinish  bool
}

func newBidiClient(messages ...*pb.DispatchMessage) *mockBidiClient {
	c := &mockBidiClient{
		messages: messages,
	}
	return c
}

func (m *mockBidiClient) IsDone() bool {
	return m.messageIndex >= len(m.messages)
}

func (m *mockBidiClient) Recv() (*pb.DispatchMessage, error) {

	if m.IsDone() {

		if m.eofOnFinish {
			return nil, io.EOF
		}
		return nil, nil
	}

	msg := m.messages[m.messageIndex]
	m.messageIndex++
	return msg, nil
}

func (m *mockBidiClient) Send(*pb.DispatchRequest) error {
	if m.IsDone() && m.eofOnFinish {
		return io.EOF
	}
	return nil
}

func (m *mockBidiClient) CloseSend() error {
	return nil
}
