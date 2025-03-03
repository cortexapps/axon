package api

import (
	"bytes"
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
}

func newHttpRequestHelper(config config.AgentConfig, logger *zap.Logger) *httpRequestHelper {
	return &httpRequestHelper{
		BaseURL: fmt.Sprintf("http://localhost:%d/cortex-api", config.HttpServerPort),
		logger:  logger.With(zap.String("component", "httpRequestHelper")),
	}
}

func (h *httpRequestHelper) makeUrl(path string) string {
	return fmt.Sprintf("%s%s", h.BaseURL, path)
}

func (h *httpRequestHelper) doRequest(req *http.Request, data *RequestBody) (*http.Response, error) {

	if data != nil {
		data.Apply(req)
	} else {
		req.Header.Set("Content-Type", "application/json")
	}

	if req.Method == "" {
		req.Method = "GET"
	}

	// replace multiple leading / in path with single /
	req.URL.Path = regexp.MustCompile("^/+").ReplaceAllString(req.URL.Path, "/")

	client := &http.Client{}
	for {
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode == http.StatusTooManyRequests {
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
			time.Sleep(retryAfterDuration)
			continue
		}

		if resp.StatusCode >= 400 {
			body := new(bytes.Buffer)
			body.ReadFrom(resp.Body)
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

type RequestBody struct {
	Body        []byte
	ContentType string
}

func (rb RequestBody) Apply(req *http.Request) {
	buf := bytes.NewBuffer(rb.Body)
	req.Body = io.NopCloser(buf)
	req.Header.Set("Content-Type", rb.ContentType)
}

func (h *httpRequestHelper) Do(method string, endpoint string, data *RequestBody) (*http.Response, error) {
	req, err := http.NewRequest(method, h.makeUrl(endpoint), nil)
	if err != nil {
		return nil, err
	}

	return h.doRequest(req, data)
}
