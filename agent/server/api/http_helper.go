package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/cortexapps/axon/config"
	"go.uber.org/zap"
)

// httpRequestHelper is a helper for making HTTP requests to the Cortex API
// which knows how to format calls to talk to the local proxy, and handles
// rate limiting and authorization.
type httpRequestHelper struct {
	BaseURL string
	logger  *zap.Logger
	client  *http.Client
}

// maxCallDuration bounds a call whose caller set no deadline, so that a
// rate limited or hung request cannot hold its caller forever.
const maxCallDuration = 5 * time.Minute

func newHttpRequestHelper(config config.AgentConfig, logger *zap.Logger) *httpRequestHelper {
	return &httpRequestHelper{
		BaseURL: fmt.Sprintf("%s/%s", config.HttpBaseUrl(), cortexApiRoot),
		logger:  logger.With(zap.String("component", "httpRequestHelper")),
		client:  &http.Client{},
	}
}

func (h *httpRequestHelper) makeUrl(path string) string {
	return fmt.Sprintf("%s%s", h.BaseURL, path)
}

func (h *httpRequestHelper) doRequest(ctx context.Context, method string, url string, data *RequestBody) (*http.Response, error) {

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, maxCallDuration)
		defer cancel()
	}

	if method == "" {
		method = "GET"
	}

	for {
		req, err := http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			return nil, err
		}

		if data != nil {
			data.Apply(req)
		} else {
			req.Header.Set("Content-Type", "application/json")
		}

		// replace multiple leading / in path with single /
		req.URL.Path = regexp.MustCompile("^/+").ReplaceAllString(req.URL.Path, "/")

		resp, err := h.client.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			retryAfter := resp.Header.Get("Retry-After")
			if retryAfter == "" {
				retryAfter = "1"
			}
			retryAfterDuration, err := time.ParseDuration(retryAfter + "s")
			if err != nil {
				retryAfterDuration = 1 * time.Second
			}
			h.logger.Warn("Rate limited. Sleeping till retry",
				zap.Duration("retry-duration", retryAfterDuration),
			)
			if err := sleepContext(ctx, retryAfterDuration); err != nil {
				return nil, fmt.Errorf("gave up retrying rate limited request: %w", err)
			}
			continue
		}

		if resp.StatusCode >= 400 {
			body := new(bytes.Buffer)
			body.ReadFrom(resp.Body)
			resp.Body.Close()
			h.logger.Error("API request failed",
				zap.Int("status-code", resp.StatusCode),
				zap.String("url", req.URL.String()),
				zap.String("method", req.Method),
				zap.String("body", body.String()),
			)
			resp.Body = io.NopCloser(bytes.NewBufferString(body.String()))
		}

		return resp, nil
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type RequestBody struct {
	Body        []byte
	ContentType string
}

func (rb RequestBody) Apply(req *http.Request) {
	buf := bytes.NewBuffer(rb.Body)
	req.Body = io.NopCloser(buf)
	req.Header.Set("Content-Type", rb.ContentType)
}

func (h *httpRequestHelper) Do(ctx context.Context, method string, endpoint string, data *RequestBody) (*http.Response, error) {
	return h.doRequest(ctx, method, h.makeUrl(endpoint), data)
}
