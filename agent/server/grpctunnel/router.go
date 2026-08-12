package grpctunnel

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"go.uber.org/zap"
)

// RouteError carries an HTTP status hint for CallCancel when a call cannot
// be routed or executed.
type RouteError struct {
	Code   int32
	Reason string
}

func (e *RouteError) Error() string { return e.Reason }

// ErrNoMatchingRule is returned when no accept file rule matches the call.
var ErrNoMatchingRule = &RouteError{Code: http.StatusNotFound, Reason: "no matching accept file rule"}

// Router matches an incoming CallStart against the rendered accept file
// rules and resolves it to a concrete upstream request.
//
// It is a thin consumer of the shared acceptfile package and holds no
// accept-file semantics of its own: matching, origin/pool resolution, and
// header/auth interpretation all live in acceptfile so both relay
// transports share one definition (design doc §9).
type Router struct {
	rules  []acceptfile.AcceptFileRuleWrapper
	pools  *acceptfile.PoolManager
	logger *zap.Logger
}

// NewRouter creates a Router over rendered accept file rules.
func NewRouter(rules []acceptfile.AcceptFileRuleWrapper, logger *zap.Logger) *Router {
	return &Router{
		rules:  rules,
		pools:  acceptfile.NewPoolManager(),
		logger: logger.Named("router"),
	}
}

// Route resolves a CallStart to a BackendRequest, or a *RouteError with an
// HTTP status hint.
func (rt *Router) Route(start *pb.CallStart) (*BackendRequest, error) {
	method := start.PseudoHeaders[":method"]
	rawPath := start.PseudoHeaders[":path"]
	if method == "" || rawPath == "" {
		return nil, &RouteError{Code: http.StatusBadRequest, Reason: "missing :method or :path"}
	}

	// Split the query off before matching; rules match on the path only.
	pathOnly, query, _ := strings.Cut(rawPath, "?")
	decodedPath, err := url.PathUnescape(pathOnly)
	if err != nil {
		return nil, &RouteError{Code: http.StatusBadRequest, Reason: fmt.Sprintf("invalid path encoding: %v", err)}
	}

	rule := acceptfile.MatchRule(rt.rules, method, decodedPath, start.Headers)
	if rule == nil {
		return nil, ErrNoMatchingRule
	}

	origin := rt.pools.ResolvePoolVars(rule.Origin())
	targetURL, err := buildTargetURL(origin, pathOnly, decodedPath, query)
	if err != nil {
		return nil, &RouteError{Code: http.StatusBadGateway, Reason: fmt.Sprintf("failed to build target URL: %v", err)}
	}

	header := make(http.Header, len(start.Headers))
	for k, v := range start.Headers {
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

	rt.logger.Debug("Routed call",
		zap.String("method", method),
		zap.String("path", decodedPath),
		zap.String("targetURL", targetURL.String()),
	)

	return &BackendRequest{
		Method: method,
		URL:    targetURL,
		Header: header,
		Kind:   start.Kind,
	}, nil
}

// applyAuth sets the Authorization header from the rule's auth block.
func applyAuth(header http.Header, auth *acceptfile.AcceptFileRuleAuth) {
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
		header.Set("Authorization", "Basic "+basicAuthValue(username, password))
	default:
		// Custom scheme: set as Authorization header.
		token := os.ExpandEnv(auth.Token)
		header.Set("Authorization", fmt.Sprintf("%s %s", auth.Scheme, token))
	}
}

// buildTargetURL joins the rule's resolved origin with the call path and
// query, preserving percent-encoded characters (e.g. %2F in GitLab project
// IDs) on the wire.
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
