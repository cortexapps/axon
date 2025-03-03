package api

import (
	"context"
	"fmt"
	"io"
	"strings"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/config"

	"go.uber.org/zap"
)

type cortexApiServer struct {
	pb.CortexApiServer
	logger *zap.Logger
	helper *httpRequestHelper
}

func NewCortexApiServer(logger *zap.Logger, config config.AgentConfig) pb.CortexApiServer {
	server := &cortexApiServer{
		logger: logger,
		helper: newHttpRequestHelper(config, logger),
	}
	return server
}

// Call is a GRPC wrapper around a simple HTTP client optimized
// for sending requests to the Cortex API.  The CallRequest allows
// for a method, path, and optional body to be sent to the Cortex API.

func (s *cortexApiServer) Call(ctx context.Context, req *pb.CallRequest) (*pb.CallResponse, error) {

	var body *RequestBody = nil

	if req.Body != "" && req.Method != "GET" {
		body = &RequestBody{
			ContentType: req.ContentType,
			Body:        []byte(req.Body),
		}

		if req.ContentType == "" {
			body.ContentType = "application/json"
		}
	}

	httpResponse, err := s.helper.Do(req.Method, req.Path, body)

	if err != nil {
		return nil, fmt.Errorf("failed to call cortex api: %w", err)
	}

	headers := make(map[string]string)
	for k, v := range httpResponse.Header {
		headers[k] = strings.Join(v, ",")
	}

	responseBody := ""
	if httpResponse.Body != nil {
		rb, err := io.ReadAll(httpResponse.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response body: %w", err)
		}
		responseBody = string(rb)
	}

	resp := &pb.CallResponse{
		StatusCode: int32(httpResponse.StatusCode),
		Status:     httpResponse.Status,
		Headers:    headers,
		Body:       responseBody,
	}

	return resp, nil
}
