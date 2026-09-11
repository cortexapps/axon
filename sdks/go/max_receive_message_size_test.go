package axon

import (
	"context"
	"fmt"
	"math"
	"net"
	"strings"
	"testing"

	pb "github.com/cortexapps/axon-go/.generated/proto/github.com/cortexapps/axon"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const oversizeBodyLength = defaultMaxReceiveMessageSize + 16*1024

type oversizeCortexApi struct {
	pb.UnimplementedCortexApiServer
}

func (s *oversizeCortexApi) Call(ctx context.Context, request *pb.CallRequest) (*pb.CallResponse, error) {
	return &pb.CallResponse{
		StatusCode: 200,
		Body:       strings.Repeat("x", oversizeBodyLength),
	}, nil
}

func startOversizeCortexApi(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := grpc.NewServer()
	pb.RegisterCortexApiServer(server, &oversizeCortexApi{})

	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(server.Stop)

	return listener.Addr().(*net.TCPAddr).Port
}

func callOversizeApi(t *testing.T, maxReceiveMessageSize int) (*pb.CallResponse, error) {
	t.Helper()

	port := startOversizeCortexApi(t)
	client := newGrpcClient("127.0.0.1", port, maxReceiveMessageSize, zap.NewNop())

	return client.api().Call(context.Background(), &pb.CallRequest{
		Method: "GET",
		Path:   "/api/v1/catalog/descriptors",
	})
}

func TestOversizeResponseRejectedByDefault(t *testing.T) {
	_, err := callOversizeApi(t, defaultMaxReceiveMessageSize)

	require.Error(t, err)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
}

func TestOversizeResponseAcceptedWithLargerLimit(t *testing.T) {
	response, err := callOversizeApi(t, 16*1024*1024)

	require.NoError(t, err)
	require.Len(t, response.Body, oversizeBodyLength)
}

func TestOversizeResponseAcceptedWithNoLimit(t *testing.T) {
	response, err := callOversizeApi(t, unlimitedMaxReceiveMessageSize)

	require.NoError(t, err)
	require.Len(t, response.Body, oversizeBodyLength)
}

func TestResolveMaxReceiveMessageSize(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		require.Equal(t, defaultMaxReceiveMessageSize, resolveMaxReceiveMessageSize())
	})

	t.Run("from environment", func(t *testing.T) {
		t.Setenv(maxReceiveMessageSizeEnvVar, "16777216")
		require.Equal(t, 16*1024*1024, resolveMaxReceiveMessageSize())
	})

	t.Run("no limit", func(t *testing.T) {
		t.Setenv(maxReceiveMessageSizeEnvVar, "-1")
		require.Equal(t, math.MaxInt32, resolveMaxReceiveMessageSize())
	})

	t.Run("invalid value panics", func(t *testing.T) {
		t.Setenv(maxReceiveMessageSizeEnvVar, "8MB")
		require.PanicsWithValue(t,
			fmt.Sprintf("%s must be a whole number of bytes, or -1 for no limit, but is %q", maxReceiveMessageSizeEnvVar, "8MB"),
			func() { resolveMaxReceiveMessageSize() },
		)
	})
}

func TestWithMaxReceiveMessageSize(t *testing.T) {
	t.Run("overrides the environment", func(t *testing.T) {
		t.Setenv(maxReceiveMessageSizeEnvVar, "16777216")

		options := defaultAgentOptions()
		WithMaxReceiveMessageSize(defaultMaxReceiveMessageSize)(options)

		require.Equal(t, defaultMaxReceiveMessageSize, options.maxReceiveMessageSize)
	})

	t.Run("no limit", func(t *testing.T) {
		options := defaultAgentOptions()
		WithMaxReceiveMessageSize(-1)(options)

		require.Equal(t, math.MaxInt32, options.maxReceiveMessageSize)
	})
}
