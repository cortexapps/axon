package acceptfile

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"go.uber.org/zap"
)

// ErrNoRoute is returned by Router.Route when no accept file rule matches
// the request.
var ErrNoRoute = errors.New("no matching accept file rule")

// InvalidRequestError marks a request that could not be routed because it
// was malformed (bad encoding, traversal, missing fields) rather than
// unmatched.
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
// wildcard-origin retargeting, target URL construction, and header/auth
// injection. It is the authoritative implementation of accept-file
// semantics; the snyk-broker reflector keeps its own copy of the wildcard
// policy until it is rerouted through this one.
type Router struct {
	rules  []AcceptFileRuleWrapper
	pools  *PoolManager
	logger *zap.Logger
}

// NewRouter creates a Router over rendered accept file rules. It resolves and
// parses every rule origin up front, so a malformed policy — a wildcard that
// authorizes too much, a port that cannot be dialed — fails here rather than
// once per request.
func NewRouter(rules []AcceptFileRuleWrapper, logger *zap.Logger) (*Router, error) {
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
		// Peek rather than rotate: validation must not consume a pool slot and
		// shift which member the first request lands on.
		origin, err := rt.resolveOrigin(rule, false)
		if err != nil {
			rt.logger.Warn("Accept file rule has an unusable origin; requests matching it "+
				"will fail to route",
				zap.String("rulePath", rule.Path()), zap.Error(err))
			continue
		}
		// A malformed wildcard origin is the one thing that stops the agent.
		// The snyk-broker path refuses it too — at render, or by panicking when
		// the reflector is off — so no working deployment has one, and treating
		// a bad family as permissive would authorize hosts nobody chose.
		parsed, err := parseOrigin(origin)
		if err != nil {
			return nil, fmt.Errorf("accept file rule %q: %w", rule.Path(), err)
		}
		// Anything else about the origin is left to fail per request, the way
		// it does on the snyk-broker path, rather than at startup.
		if parsed.url.Scheme == "" || parsed.url.Host == "" {
			rt.logger.Warn("Accept file rule has an origin with no scheme or host; "+
				"requests matching it will fail to route",
				zap.String("rulePath", rule.Path()), zap.String("origin", origin))
		}
	}
	return rt, nil
}

// Route resolves a request to a RoutedRequest. rawPath may carry a fragment, a
// query string and percent-encoded segments; rules match on the decoded path
// alone, and the encoded form is preserved on the resolved URL.
//
// Returns ErrNoRoute when no rule matches, *InvalidRequestError for malformed
// input, and ErrDestinationRejected when a matched rule does not authorize the
// destination the request named.
func (rt *Router) Route(method, rawPath string, headers map[string]string) (*RoutedRequest, error) {
	if method == "" || rawPath == "" {
		return nil, &InvalidRequestError{Reason: "missing method or path"}
	}

	// A fragment is not part of the resource, and leaving it on would let a
	// caller hide a disallowed path behind an allowed one. snyk-broker drops it
	// before matching for the same reason.
	rawPath, _, _ = strings.Cut(rawPath, "#")
	if rawPath == "" {
		return nil, &InvalidRequestError{Reason: "missing path"}
	}

	// Split the query off before matching; rules match on the path only.
	pathOnly, query, _ := strings.Cut(rawPath, "?")
	decodedPath, err := url.PathUnescape(pathOnly)
	if err != nil {
		return nil, &InvalidRequestError{Reason: fmt.Sprintf("invalid path encoding: %v", err)}
	}
	// Checked on the decoded path, so an encoded "%2e%2e" cannot slip a
	// traversal past a rule that would not have matched it spelled out.
	if !isNormalizedPath(decodedPath) {
		return nil, &InvalidRequestError{Reason: "path is not normalized"}
	}

	// Taken before matching: internal routing metadata must not survive on any
	// path — forwarded, logged or rejected — and must not be able to select a
	// rule either.
	requestedTargets, headers := takeTargetHosts(headers)

	rule := MatchRule(rt.rules, method, decodedPath, headers)
	if rule == nil {
		return nil, ErrNoRoute
	}

	// A rule that names a segment with ${VAR} rewrites it on the way out. Both
	// spellings of the path go through it: the escaped form is what travels,
	// and it has to keep agreeing with the decoded form the rule matched on.
	// Guarded so a rule without a placeholder — every rule Axon ships — costs
	// nothing beyond the match MatchRule already did.
	if rulePath := rule.Path(); strings.Contains(rulePath, "${") {
		if rewritten, ok := matchPath(rulePath, decodedPath); ok {
			decodedPath = rewritten
		}
		if rewritten, ok := matchPath(rulePath, pathOnly); ok {
			pathOnly = rewritten
		}
	}

	origin, err := rt.resolveOrigin(*rule, true)
	if err != nil {
		return nil, err
	}
	parsed, err := parseOrigin(origin)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve origin: %w", err)
	}

	// A tunnel call carries no per-request routing once upgraded, so a family
	// has no authority to upgrade against.
	if parsed.isWildcard() && isWebSocketUpgrade(headers) {
		rt.logger.Error("WebSocket upgrade is not supported for a wildcard origin",
			zap.String("origin", origin))
		return nil, fmt.Errorf("%w: wildcard origin cannot carry a WebSocket upgrade", ErrDestinationRejected)
	}

	// The reason is safe to log; the requested value is not.
	targetHost, err := parsed.resolveTargetHost(requestedTargets)
	if err != nil {
		rt.logger.Error("Rejected destination",
			zap.String("origin", origin),
			zap.Error(err))
		return nil, err
	}

	targetURL, err := buildTargetURL(parsed.url, targetHost, pathOnly, decodedPath, query)
	if err != nil {
		return nil, fmt.Errorf("failed to build target URL: %w", err)
	}

	header := make(http.Header, len(headers))
	for k, v := range headers {
		header.Set(k, v)
	}

	// Inject rule headers (overrides incoming).
	if ruleHeaders := rule.Headers(); ruleHeaders != nil {
		// A credential provider that fails has to stop the request rather than
		// let its placeholder travel upstream as the credential — the refusal
		// then comes back as an authorization failure and names the wrong
		// culprit, which is the thing #127 set out to stop. The reflector's
		// serve() answers 502 here; returning an error lands in the default
		// arm of grpctunnel's RouteError mapping, which is also a 502, so both
		// transports refuse the same way.
		resolved, err := ruleHeaders.ToStringMap()
		if err != nil {
			rt.logger.Error("Credential provider failed",
				zap.String("rulePath", rule.Path()),
				zap.Error(err),
			)
			return nil, fmt.Errorf("credential provider failed: %w", err)
		}
		for k, v := range resolved {
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

// resolveOrigin turns a rule's raw origin into a concrete URL string, rotating
// any ${VAR} that names a pool. advance is false for validation, which must not
// consume a slot.
func (rt *Router) resolveOrigin(rule AcceptFileRuleWrapper, advance bool) (string, error) {
	raw := rule.RawOrigin()
	if raw == "" {
		return "", fmt.Errorf("rule has no origin")
	}
	return defaultScheme(rt.pools.resolveVars(raw, advance))
}

// defaultScheme fills in https for an origin written without one, which the
// accept-file format allows (${GITHUB:github.com}).
func defaultScheme(origin string) (string, error) {
	asURL, err := url.Parse(origin)
	if err != nil {
		return "", fmt.Errorf("invalid origin %q: %w", origin, err)
	}
	if asURL.Scheme != "" {
		return origin, nil
	}
	asURL.Scheme = "https"
	return asURL.String(), nil
}

// buildTargetURL joins the resolved origin with the request path and query,
// preserving percent-encoded characters (e.g. %2F in GitLab project IDs) on the
// wire. targetHost, when set, replaces the origin's host — the origin keeps
// deciding the port.
func buildTargetURL(declared *url.URL, targetHost, escapedPath, decodedPath, query string) (*url.URL, error) {
	if declared.Scheme == "" || declared.Host == "" {
		return nil, fmt.Errorf("origin %q has no scheme or host", declared.String())
	}

	joinEscaped := joinPath(declared.EscapedPath(), escapedPath)
	joinDecoded := joinPath(declared.Path, decodedPath)

	u := *declared
	if targetHost != "" {
		if port := declared.Port(); port != "" {
			targetHost = targetHost + ":" + port
		}
		u.Host = targetHost
	}
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
