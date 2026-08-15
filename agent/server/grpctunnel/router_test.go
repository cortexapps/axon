package grpctunnel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func makeRouterRules(t *testing.T, rules string) []acceptfile.AcceptFileRuleWrapper {
	t.Helper()
	cfg := config.AgentConfig{
		HttpServerPort: 8080,
		PluginDirs:     []string{},
	}
	af, err := acceptfile.NewAcceptFile([]byte(rules), cfg, zap.NewNop())
	require.NoError(t, err)
	rendered, err := af.Render(zap.NewNop())
	require.NoError(t, err)
	af2, err := acceptfile.NewAcceptFile(rendered, cfg, zap.NewNop())
	require.NoError(t, err)

	// Filter out the axon route added by render.
	var filtered []acceptfile.AcceptFileRuleWrapper
	for _, r := range af2.Wrapper().PrivateRules() {
		if r.Path() != "/__axon/*" {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

func callStart(method, path string, headers map[string]string) *pb.CallStart {
	return &pb.CallStart{
		PseudoHeaders: map[string]string{":method": method, ":path": path},
		Headers:       headers,
	}
}

// doCall routes a CallStart and executes it through the HttpBackend,
// returning status, body, and response headers.
func doCall(t *testing.T, router *Router, start *pb.CallStart, body io.Reader) (int, string, http.Header) {
	t.Helper()
	breq, err := router.Route(start)
	require.NoError(t, err)
	breq.Body = body

	backend := NewHttpBackend(&http.Client{}, zap.NewNop())
	resp, err := backend.Do(context.Background(), breq)
	require.NoError(t, err)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(got), resp.Header
}

func TestRouterBackend_BasicRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "/api/v1/repos", r.URL.Path)
		w.Header().Set("X-Test", "response-header")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"repos": []}`))
	}))
	defer server.Close()

	rulesJSON := fmt.Sprintf(`{
		"private": [
			{
				"method": "GET",
				"path": "/api/v1/repos",
				"origin": "%s"
			}
		]
	}`, server.URL)

	router := NewRouter(makeRouterRules(t, rulesJSON), zap.NewNop())
	status, body, headers := doCall(t, router, callStart("GET", "/api/v1/repos", nil), nil)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, `{"repos": []}`, body)
	assert.Equal(t, "response-header", headers.Get("X-Test"))
}

func TestRouterBackend_QueryStringForwarded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/search", r.URL.Path)
		assert.Equal(t, "q=foo&page=2", r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	rulesJSON := fmt.Sprintf(`{
		"private": [{"method": "GET", "path": "/api/*", "origin": "%s"}]
	}`, server.URL)

	router := NewRouter(makeRouterRules(t, rulesJSON), zap.NewNop())
	status, _, _ := doCall(t, router, callStart("GET", "/api/search?q=foo&page=2", nil), nil)
	assert.Equal(t, http.StatusOK, status)
}

func TestRouterBackend_EncodedSlashPreserved(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// %2F must survive on the wire (GitLab project IDs).
		assert.Equal(t, "/api/v4/projects/group%2Fproject", r.URL.EscapedPath())
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	rulesJSON := fmt.Sprintf(`{
		"private": [{"method": "any", "path": "/api/*", "origin": "%s"}]
	}`, server.URL)

	router := NewRouter(makeRouterRules(t, rulesJSON), zap.NewNop())
	status, _, _ := doCall(t, router, callStart("GET", "/api/v4/projects/group%2Fproject", nil), nil)
	assert.Equal(t, http.StatusOK, status)
}

func TestRouter_NoMatchingRule(t *testing.T) {
	rulesJSON := `{
		"private": [
			{
				"method": "GET",
				"path": "/api/v1/repos",
				"origin": "https://example.com"
			}
		]
	}`

	router := NewRouter(makeRouterRules(t, rulesJSON), zap.NewNop())
	_, err := router.Route(callStart("GET", "/unknown/path", nil))
	require.Error(t, err)
	var re *RouteError
	require.ErrorAs(t, err, &re)
	assert.Equal(t, int32(http.StatusNotFound), re.Code)
}

func TestRouterBackend_BearerAuth(t *testing.T) {
	t.Setenv("MY_TOKEN", "secret-token-123")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer secret-token-123", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	rulesJSON := fmt.Sprintf(`{
		"private": [
			{
				"method": "GET",
				"path": "/api/*",
				"origin": "%s",
				"auth": {
					"scheme": "bearer",
					"token": "${MY_TOKEN}"
				}
			}
		]
	}`, server.URL)

	router := NewRouter(makeRouterRules(t, rulesJSON), zap.NewNop())
	status, _, _ := doCall(t, router, callStart("GET", "/api/repos", nil), nil)
	assert.Equal(t, http.StatusOK, status)
}

func TestRouterBackend_BasicAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		assert.True(t, ok)
		assert.Equal(t, "myuser", user)
		assert.Equal(t, "mypass", pass)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	rulesJSON := fmt.Sprintf(`{
		"private": [
			{
				"method": "POST",
				"path": "/api/*",
				"origin": "%s",
				"auth": {
					"scheme": "basic",
					"username": "myuser",
					"password": "mypass"
				}
			}
		]
	}`, server.URL)

	router := NewRouter(makeRouterRules(t, rulesJSON), zap.NewNop())
	status, _, _ := doCall(t, router, callStart("POST", "/api/data", nil), strings.NewReader(`{"key":"value"}`))
	assert.Equal(t, http.StatusOK, status)
}

func TestRouterBackend_RuleHeaderInjection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "injected-value", r.Header.Get("X-Custom"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	rulesJSON := fmt.Sprintf(`{
		"private": [
			{
				"method": "GET",
				"path": "/api/*",
				"origin": "%s",
				"headers": {
					"X-Custom": "injected-value"
				}
			}
		]
	}`, server.URL)

	router := NewRouter(makeRouterRules(t, rulesJSON), zap.NewNop())
	status, _, _ := doCall(t, router, callStart("GET", "/api/repos", map[string]string{"x-custom": "caller-value"}), nil)
	assert.Equal(t, http.StatusOK, status)
}

func TestRouterBackend_StreamedRequestBody(t *testing.T) {
	// The backend must pass the body reader through without buffering: the
	// upstream sees bytes that are written after the request has started.
	bodyR, bodyW := io.Pipe()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.Equal(t, "part-1part-2", string(got))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	rulesJSON := fmt.Sprintf(`{
		"private": [{"method": "POST", "path": "/api/*", "origin": "%s"}]
	}`, server.URL)

	go func() {
		bodyW.Write([]byte("part-1"))
		bodyW.Write([]byte("part-2"))
		bodyW.Close()
	}()

	router := NewRouter(makeRouterRules(t, rulesJSON), zap.NewNop())
	status, _, _ := doCall(t, router, callStart("POST", "/api/upload", nil), bodyR)
	assert.Equal(t, http.StatusOK, status)
}
