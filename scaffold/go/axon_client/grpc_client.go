package axon

import (
	"fmt"

	pb "github.com/cortexapps/axon-go/.generated/proto/github.com/cortexapps/axon"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// grpcClient is an interface to abstract the grpc client
// to make this system more testable
type grpcClient interface {
	api() pb.CortexApiClient
	agent() pb.AxonAgentClient
}

type grpcClientImpl struct {
	host          string
	port          int
	conn          *grpc.ClientConn
	stub          pb.AxonAgentClient
	apiClientStub pb.CortexApiClient

	logger *zap.Logger
}

func newGrpcClient(host string, port int, logger *zap.Logger) grpcClient {
	return &grpcClientImpl{
		host:   host,
		port:   port,
		logger: logger,
	}
}

func (c *grpcClientImpl) getConnection() *grpc.ClientConn {

	if c.conn == nil {

		conn, err := grpc.NewClient(
			fmt.Sprintf("%s:%d", c.host, c.port),
			grpc.WithTransportCredentials(insecure.NewCredentials()))

		if err != nil {
			c.logger.Error("failed to create connection to agent", zap.Error(err))
			return nil
		}
		c.conn = conn
	}
	return c.conn
}

func (c *grpcClientImpl) agent() pb.AxonAgentClient {
	if c.stub == nil {
		conn := c.getConnection()
		if conn == nil {
			return nil
		}
		c.stub = pb.NewAxonAgentClient(conn)
	}
	return c.stub
}

func (c *grpcClientImpl) api() pb.CortexApiClient {
	if c.apiClientStub == nil {
		conn := c.getConnection()
		if conn == nil {
			return nil
		}
		c.apiClientStub = pb.NewCortexApiClient(conn)
	}
	return c.apiClientStub
}
