package grpctunnel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"go.uber.org/zap"
)

// BackendRequest is a routed call ready to execute against an upstream.
type BackendRequest struct {
	Method string
	URL    *url.URL
	Header http.Header
	// Body streams the request body as CallData frames arrive. Nil for
	// bodyless requests.
	Body io.Reader
	Kind pb.CallStart_Kind
}

// BackendResponse is a streaming upstream response.
type BackendResponse struct {
	StatusCode int
	Header     http.Header
	// Body streams the response; the caller must Close it.
	Body io.ReadCloser
	// Trailer returns the response trailers; valid only after Body has
	// been read to EOF. May return nil.
	Trailer func() http.Header
}

// Backend executes routed calls against an upstream. HttpBackend is the
// only implementation today; a future GrpcBackend implements the same
// interface for gRPC upstreams (design doc §7.4).
type Backend interface {
	Do(ctx context.Context, req *BackendRequest) (*BackendResponse, error)
}

// HttpBackend executes calls with the shared *http.Client, which already
// carries the agent's proxy, CA-cert, and TLS configuration.
type HttpBackend struct {
	client *http.Client
	logger *zap.Logger
}

// NewHttpBackend creates an HttpBackend over the shared HTTP client.
func NewHttpBackend(client *http.Client, logger *zap.Logger) *HttpBackend {
	return &HttpBackend{client: client, logger: logger.Named("http-backend")}
}

// Do executes the request, streaming the body in both directions — the
// request body is read as it arrives from the tunnel, and the response
// body is returned as a reader rather than buffered.
func (b *HttpBackend) Do(ctx context.Context, req *BackendRequest) (*BackendResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL.String(), req.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header = req.Header.Clone()
	if httpReq.Header == nil {
		httpReq.Header = make(http.Header)
	}
	// The tunnel carries end-to-end headers only; hop-by-hop headers from
	// the original caller must not leak into the upstream connection.
	for _, h := range []string{"connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade", "te"} {
		httpReq.Header.Del(h)
	}
	httpReq.Host = req.URL.Host

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request execution failed: %w", err)
	}

	return &BackendResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       resp.Body,
		Trailer:    func() http.Header { return resp.Trailer },
	}, nil
}
