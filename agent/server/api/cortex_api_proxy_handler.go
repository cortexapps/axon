package api

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/cortexapps/axon/config"
	cortex_http "github.com/cortexapps/axon/server/http"
	"go.uber.org/zap"
)

// To invoke the cortex API, calls go against
//
// http://localhost/cortex-api/api/v1/catalog...
//
// Which will be proxied as a call to the actual Cortex API (minus /cortex-api)
const cortexApiRoot = "cortex-api"
const cortexApiPathRoot = "/cortex-api/"

func NewApiProxyHandler(config config.AgentConfig, logger *zap.Logger) cortex_http.RegisterableHandler {
	targetURL, err := url.Parse(config.CortexApiBaseUrl)
	if err != nil {
		panic(fmt.Errorf("failed to parse target URL: %w", err))
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	// The proxy needs to override the Host and the URL host to not get erroneous 404s
	// https://stackoverflow.com/questions/23164547/golang-reverseproxy-not-working
	// https://github.com/golang/go/issues/14413
	defaultDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		defaultDirector(req)
		req.Host = targetURL.Host
	}
	return &apiProxyHandler{
		proxy:  proxy,
		config: config,
		logger: logger,
	}
}

type apiProxyHandler struct {
	io.Closer
	config config.AgentConfig
	proxy  *httputil.ReverseProxy
	logger *zap.Logger
}

func (a *apiProxyHandler) Path() string {
	return cortexApiPathRoot
}

func (a *apiProxyHandler) RegisterRoutes(mux *http.ServeMux) error {
	mux.Handle(cortexApiPathRoot, a)
	mux.Handle("/", a)
	return nil
}

func (a *apiProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	if strings.HasPrefix(r.URL.Path, cortexApiPathRoot) {
		r.URL.Path = r.URL.Path[len(cortexApiPathRoot):]
	} else if r.Host != cortexApiRoot {
		w.WriteHeader(404)
		return
	}

	// Our main proxy function, which
	// 1. Handles dry run mode
	// 2. Adds the Cortex API token to the request
	// 3. Retries the request if it is rate limited
	r.Header.Set("Authorization", fmt.Sprintf("Bearer %s", a.config.CortexApiToken))

	if a.config.DryRun {
		a.logger.Info("DRY RUN", zap.String("method", r.Method), zap.Any("path", r.URL))
		a.logger.Info("\tHeaders", zap.Any("headers", r.Header))
		if r.Body != nil {
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				a.logger.Warn("Failed to read body: %v", zap.Error(err))
			} else if len(bodyBytes) > 0 {
				a.logger.Info("Body", zap.String("body", string(bodyBytes)))
				fmt.Printf("\tBody: %s\n", string(bodyBytes))
			}
			defer r.Body.Close()

		}
		w.WriteHeader(http.StatusOK)
		return
	}

	for {

		if r.Context().Err() != nil {
			a.logger.Warn("Request cancelled", zap.String("url", r.URL.String()))
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}

		// add an ability to capture and retry this request
		request := r.Clone(r.Context())
		recorder := &captureResponseWriter{
			headers: make(http.Header),
		}

		// Forward the request to the Cortex API
		a.proxy.ServeHTTP(recorder, request)

		if wait := a.retryAfter(recorder); wait > 0 {
			time.Sleep(wait)
			continue
		}

		if recorder.Code >= 400 {

			a.logger.Error("API request failed",
				zap.Int("status-code", recorder.Code),
				zap.String("url", request.URL.String()),
				zap.String("method", request.Method),
				zap.String("body", recorder.bodyAsString()),
			)
		}
		recorder.CopyTo(w)
		return
	}
}

func (a *apiProxyHandler) retryAfter(recorder *captureResponseWriter) time.Duration {

	switch recorder.Code {
	case http.StatusTooManyRequests:
		retryAfter := recorder.Header().Get("Retry-After")
		if retryAfter == "" {
			retryAfter = "1"
		}
		retryAfterDuration, err := time.ParseDuration(retryAfter + "s")
		if err != nil {
			a.logger.Warn("Failed to parse Retry-After header", zap.String("retry-after", retryAfter), zap.Error(err))
			retryAfterDuration = 1 * time.Second
		}
		a.logger.Warn("Rate limited. Sleeping till retry",
			zap.Duration("retry-duration", retryAfterDuration),
		)

		return retryAfterDuration
	}
	return 0
}

//
// Proxy server setup
//

func (a *apiProxyHandler) RegisterHandler(mux *http.ServeMux) {
	mux.Handle(cortexApiPathRoot, a)
}

// captureResponseWriter is a http.ResponseWriter that captures the response, body and headers from
// the proxied request, so that we can inspect and then retry the request if necessary.
//
// Once the response is not retryable, it can be copied to the original response writer.
type captureResponseWriter struct {
	Code    int
	headers http.Header
	body    bytes.Buffer
}

func (c *captureResponseWriter) Header() http.Header {
	return c.headers
}

func (c *captureResponseWriter) WriteHeader(statusCode int) {
	c.Code = statusCode
}

func (c *captureResponseWriter) Write(b []byte) (int, error) {
	return c.body.Write(b)
}

// bodyAsString returns the response body as a string, decompressing it if necessary
// it does not consume or close the body reader
func (c *captureResponseWriter) bodyAsString() string {

	var err error
	bodyBytes := c.body.Bytes()
	var body io.Reader = bytes.NewReader(bodyBytes)
	if strings.EqualFold(c.Header().Get("Content-Encoding"), "gzip") {
		body, err = gzip.NewReader(body)
		if err != nil {
			return fmt.Sprintf("failed to create gzip reader to read response: %v", err)
		}
	}
	bb, err := io.ReadAll(body)
	if err != nil {
		return fmt.Sprintf("failed to read response body: %v", err)
	}
	return string(bb)
}

func (c *captureResponseWriter) CopyTo(w http.ResponseWriter) {
	for k, v := range c.headers {
		for _, vv := range v {
			w.Header().Set(k, vv)
		}
	}
	w.WriteHeader(c.Code)
	w.Write(c.body.Bytes())

}
