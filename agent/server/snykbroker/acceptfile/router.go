package acceptfile

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"go.uber.org/zap"
)

// ErrNoRoute is returned by Router.Route when no accept file rule matches
// the request.
var ErrNoRoute = errors.New("no matching accept file rule")

// InvalidRequestError marks a request that could not be routed because it
// was malformed (bad encoding, missing fields) rather than unmatched.
type InvalidRequestError struct {
	Reason string
}

func (e *InvalidRequestError) Error() string { return e.Reason }

// RoutedRequest is the transport-agnostic result of routing a request
// through the accept file: the resolved upstream URL and the outgoing
// headers with rule headers and auth injected.
type RoutedRequest struct {
	Method string
	URL    *url.URL
	Header http.Header
}

// Router resolves incoming requests (method + path + headers) against
// rendered accept file rules: rule matching, origin/pool resolution,
// target URL construction, and header/auth injection. It is shared by
// every relay transport (the gRPC tunnel today, the snyk-broker reflector
// as it migrates) so accept-file semantics have exactly one
// implementation.
type Router struct {
	rules  []AcceptFileRuleWrapper
	pools  *PoolManager
	logger *zap.Logger
}

// NewRouter creates a Router over rendered accept file rules.
func NewRouter(rules []AcceptFileRuleWrapper, logger *zap.Logger) *Router {
	if logger == nil {
		logger = zap.NewNop()
	}
	rt := &Router{
		rules:  rules,
		pools:  NewPoolManager(),
		logger: logger.Named("accept-router"),
	}
	for _, rule := range rules {
		warnUnsupportedRule(rule.dict, rt.logger)
	}
	return rt
}

// Route resolves a request to a RoutedRequest. rawPath may carry a query
// string and percent-encoded segments; rules match on the decoded path
// without the query, and the encoded form is preserved on the resolved
// URL. Returns ErrNoRoute when no rule matches and *InvalidRequestError
// for malformed input.
func (rt *Router) Route(method, rawPath string, headers map[string]string) (*RoutedRequest, error) {
	if method == "" || rawPath == "" {
		return nil, &InvalidRequestError{Reason: "missing method or path"}
	}

	// Split the query off before matching; rules match on the path only.
	pathOnly, query, _ := strings.Cut(rawPath, "?")
	decodedPath, err := url.PathUnescape(pathOnly)
	if err != nil {
		return nil, &InvalidRequestError{Reason: fmt.Sprintf("invalid path encoding: %v", err)}
	}

	rule := MatchRule(rt.rules, method, decodedPath, headers)
	if rule == nil {
		return nil, ErrNoRoute
	}

	origin := rt.pools.ResolvePoolVars(rule.Origin())
	targetURL, err := buildTargetURL(origin, pathOnly, decodedPath, query)
	if err != nil {
		return nil, fmt.Errorf("failed to build target URL: %w", err)
	}

	header := make(http.Header, len(headers))
	for k, v := range headers {
		header.Set(k, v)
	}

	// Inject rule headers (overrides incoming).
	if ruleHeaders := rule.Headers(); ruleHeaders != nil {
		for k, v := range ruleHeaders.ToStringMap() {
			header.Set(k, v)
		}
	}

	// Inject auth.
	applyAuth(header, rule.Auth())

	rt.logger.Debug("Routed request",
		zap.String("method", method),
		zap.String("path", decodedPath),
		zap.String("targetURL", targetURL.String()),
	)

	return &RoutedRequest{
		Method: method,
		URL:    targetURL,
		Header: header,
	}, nil
}

// applyAuth sets the Authorization header from the rule's auth block.
func applyAuth(header http.Header, auth *AcceptFileRuleAuth) {
	if auth == nil {
		return
	}
	switch strings.ToLower(auth.Scheme) {
	case "bearer", "token":
		token := os.ExpandEnv(auth.Token)
		header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	case "basic":
		username := os.ExpandEnv(auth.Username)
		password := os.ExpandEnv(auth.Password)
		header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
	default:
		// Custom scheme: set as Authorization header.
		token := os.ExpandEnv(auth.Token)
		header.Set("Authorization", fmt.Sprintf("%s %s", auth.Scheme, token))
	}
}

// buildTargetURL joins the rule's resolved origin with the request path
// and query, preserving percent-encoded characters (e.g. %2F in GitLab
// project IDs) on the wire.
func buildTargetURL(origin, escapedPath, decodedPath, query string) (*url.URL, error) {
	parsed, err := url.Parse(origin)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("origin %q has no scheme or host", origin)
	}

	joinEscaped := joinPath(parsed.EscapedPath(), escapedPath)
	joinDecoded := joinPath(parsed.Path, decodedPath)

	u := *parsed
	u.Path = joinDecoded
	if joinEscaped != joinDecoded {
		u.RawPath = joinEscaped
	} else {
		u.RawPath = ""
	}
	u.RawQuery = query
	return &u, nil
}

func joinPath(base, p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if base == "" || base == "/" {
		return p
	}
	return strings.TrimRight(base, "/") + p
}

// urlPathUnescape is a test seam over url.PathUnescape.
func urlPathUnescape(s string) (string, error) {
	return url.PathUnescape(s)
}
