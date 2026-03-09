package requestexecutor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"go.uber.org/zap"
)

// ExecutorResponse is the result of executing an HTTP request through a matched accept file rule.
type ExecutorResponse struct {
	StatusCode int
	Headers    map[string]string
	Body       []byte
}

// RequestExecutor applies accept file rules to execute HTTP requests.
// It matches incoming requests against rules, rewrites URLs, injects headers/auth,
// and executes the request against the target origin.
type RequestExecutor interface {
	Execute(ctx context.Context, method, path string, headers map[string]string, body []byte) (*ExecutorResponse, error)
}

// ErrNoMatchingRule is returned when no accept file rule matches the request.
var ErrNoMatchingRule = fmt.Errorf("no matching accept file rule")

type requestExecutor struct {
	rules      []acceptfile.AcceptFileRuleWrapper
	logger     *zap.Logger
	httpClient *http.Client
	pools      *PoolManager
}

// NewRequestExecutor creates a new RequestExecutor from rendered accept file rules.
// The httpClient parameter should be the shared *http.Client from DI, which already
// handles proxy (http.ProxyFromEnvironment), CA certs (including directories), and TLS config.
func NewRequestExecutor(rules []acceptfile.AcceptFileRuleWrapper, httpClient *http.Client, logger *zap.Logger) RequestExecutor {
	return &requestExecutor{
		rules:      rules,
		logger:     logger.Named("request-executor"),
		httpClient: httpClient,
		pools:      NewPoolManager(),
	}
}

func (e *requestExecutor) Execute(ctx context.Context, method, path string, headers map[string]string, body []byte) (*ExecutorResponse, error) {
	rule := MatchRule(e.rules, method, path)
	if rule == nil {
		return nil, ErrNoMatchingRule
	}

	origin := e.resolveOrigin(rule.Origin())
	targetURL, err := buildTargetURL(origin, path)
	if err != nil {
		return nil, fmt.Errorf("failed to build target URL: %w", err)
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Copy incoming headers first.
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// Inject rule headers (overrides incoming).
	ruleHeaders := rule.Headers()
	if ruleHeaders != nil {
		resolved := ruleHeaders.ToStringMap()
		for k, v := range resolved {
			req.Header.Set(k, v)
		}
	}

	// Inject auth.
	e.applyAuth(req, rule.Auth())

	// Set Host header to target.
	parsedOrigin, _ := url.Parse(origin)
	if parsedOrigin != nil {
		req.Host = parsedOrigin.Host
	}

	e.logger.Debug("Executing request",
		zap.String("method", method),
		zap.String("path", path),
		zap.String("targetURL", targetURL),
	)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request execution failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	respHeaders := make(map[string]string, len(resp.Header))
	for k, v := range resp.Header {
		respHeaders[k] = strings.Join(v, ", ")
	}

	return &ExecutorResponse{
		StatusCode: resp.StatusCode,
		Headers:    respHeaders,
		Body:       respBody,
	}, nil
}

// resolveOrigin resolves _POOL variables in the origin URL via environment expansion and pool rotation.
func (e *requestExecutor) resolveOrigin(origin string) string {
	// The origin may contain env vars like ${GITHUB_API} or pool vars like ${GITHUB_API_POOL}.
	// After preprocessing, env vars are already expanded in the origin string.
	// We need to check if the resolved value is a comma-separated pool.
	return e.pools.ResolvePoolVars(origin)
}

func (e *requestExecutor) applyAuth(req *http.Request, auth *acceptfile.AcceptFileRuleAuth) {
	if auth == nil {
		return
	}
	switch strings.ToLower(auth.Scheme) {
	case "bearer", "token":
		token := os.ExpandEnv(auth.Token)
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	case "basic":
		username := os.ExpandEnv(auth.Username)
		password := os.ExpandEnv(auth.Password)
		req.SetBasicAuth(username, password)
	default:
		// Custom scheme: set as Authorization header.
		token := os.ExpandEnv(auth.Token)
		req.Header.Set("Authorization", fmt.Sprintf("%s %s", auth.Scheme, token))
	}
}

func buildTargetURL(origin, requestPath string) (string, error) {
	parsed, err := url.Parse(origin)
	if err != nil {
		return "", err
	}
	// Append the request path to the origin's path.
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = requestPath
	} else {
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + strings.TrimLeft(requestPath, "/")
	}
	return parsed.String(), nil
}
