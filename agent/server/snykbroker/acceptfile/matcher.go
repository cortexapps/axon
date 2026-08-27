package acceptfile

import (
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
)

// Rule matching runs on the gRPC tunnel path only.
//
// Despite living under snykbroker/, nothing in this file is consulted when the
// relay runs through snyk-broker: there the Node broker matches the rendered
// accept file itself, and the agent only hands it the file. The one non-test
// caller is acceptfile.Router, which the tunnel client builds.
//
// It is written to agree with the broker's matcher anyway — path-to-regexp
// wildcards, the GET default, traversal and fragment handling — because
// enabling the tunnel switches deployments that are running on snyk-broker
// today, and a rule that selected one origin there has to select the same one
// here.

// MatchRule finds the first accept file rule that matches the given HTTP method, path, and headers.
// Returns nil if no rule matches.
func MatchRule(rules []AcceptFileRuleWrapper, method, requestPath string, headers ...map[string]string) *AcceptFileRuleWrapper {
	var reqHeaders map[string]string
	if len(headers) > 0 {
		reqHeaders = headers[0]
	}

	for i := range rules {
		rule := &rules[i]
		if matchesMethod(rule.Method(), method) && matchesPath(rule.Path(), requestPath) && matchesValid(rule.Valid(), reqHeaders) {
			return rule
		}
	}
	return nil
}

// matchesValid checks if the request headers satisfy the rule's "valid" requirements.
// If no requirements are specified, returns true.
func matchesValid(requirements []ValidHeaderRequirement, headers map[string]string) bool {
	if len(requirements) == 0 {
		return true
	}

	for _, req := range requirements {
		headerValue, exists := getHeaderCaseInsensitive(headers, req.Header)
		if !exists {
			return false
		}

		// Check if the header value matches one of the allowed values.
		if len(req.Values) > 0 {
			matched := false
			for _, allowedValue := range req.Values {
				if strings.EqualFold(headerValue, allowedValue) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	}

	return true
}

// matchesMethod checks if the rule method matches the request method.
// "any" matches all methods. A rule with no method means GET, as it does in
// snyk-broker — leaving it unmatched would make the rule silently dead.
func matchesMethod(ruleMethod, requestMethod string) bool {
	if ruleMethod == "" {
		ruleMethod = "GET"
	}
	if strings.EqualFold(ruleMethod, "any") {
		return true
	}
	return strings.EqualFold(ruleMethod, requestMethod)
}

// pathPatterns caches the compiled form of each rule path. Patterns come from
// the accept file, so the set is small and fixed after load.
var pathPatterns sync.Map

// rePathVar matches a ${VAR} reference in a rule path. preProcessContent has
// already resolved ${VAR:default} and rewritten ${plugin:...}, so what reaches
// here is a plain environment reference.
var rePathVar = regexp.MustCompile(`\$\{([^}]+)\}`)

// pathPattern is a compiled rule path.
//
// vars holds, per capture group, the environment variable that group stands
// for, or "" for a "*" group. snyk-broker treats ${VAR} in a path as a segment
// placeholder — it matches any one segment and the configured value is
// substituted into the outgoing URL — so the group indexes have to survive
// compilation to do the same.
type pathPattern struct {
	re   *regexp.Regexp
	vars []string
}

// matchesPath reports whether the request path matches the rule's path pattern.
//
// "*" matches any run of characters, separators included, which is what
// path-to-regexp — the matcher snyk-broker compiles rules with — does. Go's
// path.Match stops "*" at a separator, which silently breaks every rule with an
// infix wildcard: accept.gitlab.json's "/*/info/refs" would miss the nested
// group paths that are the common case on GitLab.
func matchesPath(pattern, requestPath string) bool {
	_, ok := matchPath(pattern, requestPath)
	return ok
}

// matchPath matches the request path and returns the path to send upstream.
//
// The two differ only when the rule pins a segment with ${VAR}: the pattern
// matches whatever the caller sent there, and the configured value replaces it
// on the way out. That is snyk-broker's behaviour, quirks included — the rule
// is a rewrite rather than a filter — and a file relying on it has to keep
// working when its deployment moves onto the tunnel.
func matchPath(pattern, requestPath string) (string, bool) {
	if pattern == "" {
		return "", false
	}

	requestPath = "/" + strings.TrimLeft(requestPath, "/")
	compiled := pathRegexp(pattern)

	loc := compiled.re.FindStringSubmatchIndex(requestPath)
	if loc == nil {
		return "", false
	}
	return compiled.rewrite(requestPath, loc), true
}

// rewrite substitutes the configured value for each ${VAR} group. An unset
// variable leaves the caller's segment alone, as it does in snyk-broker, rather
// than collapsing the segment to nothing.
func (p pathPattern) rewrite(requestPath string, loc []int) string {
	var out strings.Builder
	last := 0
	for group, name := range p.vars {
		if name == "" {
			continue
		}
		value := os.Getenv(name)
		if value == "" {
			continue
		}
		start, end := loc[2*(group+1)], loc[2*(group+1)+1]
		if start < 0 {
			continue
		}
		out.WriteString(requestPath[last:start])
		out.WriteString(value)
		last = end
	}
	if last == 0 {
		return requestPath
	}
	out.WriteString(requestPath[last:])
	return out.String()
}

func pathRegexp(pattern string) pathPattern {
	if cached, ok := pathPatterns.Load(pattern); ok {
		return cached.(pathPattern)
	}
	compiled := compilePathPattern(pattern)
	pathPatterns.Store(pattern, compiled)
	return compiled
}

// compilePathPattern builds the equivalent of path-to-regexp's output for the
// subset of its syntax Axon accept files use: literal segments, "*", and
// ${VAR}. The trailing "/?" mirrors path-to-regexp's optional trailing slash,
// so "/graphql" also matches "/graphql/" — spelled without the lookahead it
// uses, which RE2 does not have and which the anchor makes redundant anyway.
func compilePathPattern(pattern string) pathPattern {
	pattern = "/" + strings.TrimLeft(pattern, "/")

	var b strings.Builder
	var vars []string
	b.WriteString("^")

	// Split on ${VAR} first, then on "*" within each literal run, so both kinds
	// of placeholder become capture groups in source order.
	last := 0
	for _, m := range rePathVar.FindAllStringSubmatchIndex(pattern, -1) {
		writeStarSplit(&b, &vars, pattern[last:m[0]])
		b.WriteString("([^/]+)")
		vars = append(vars, pattern[m[2]:m[3]])
		last = m[1]
	}
	writeStarSplit(&b, &vars, pattern[last:])
	b.WriteString("/?$")

	// The pattern comes from the accept file and every literal run is escaped
	// above, so a compile failure is not reachable; refuse to match rather than
	// panic on the request path if it ever becomes so.
	re, err := regexp.Compile(b.String())
	if err != nil {
		return pathPattern{re: regexp.MustCompile("$.^")}
	}
	return pathPattern{re: re, vars: vars}
}

// writeStarSplit appends a literal run, turning each "*" into a capture group.
func writeStarSplit(b *strings.Builder, vars *[]string, run string) {
	for i, literal := range strings.Split(run, "*") {
		if i > 0 {
			b.WriteString("(.*)")
			*vars = append(*vars, "")
		}
		b.WriteString(regexp.QuoteMeta(literal))
	}
}

// isNormalizedPath reports whether the path is already in its canonical form.
//
// snyk-broker refuses any request whose path normalization changes it, which
// blocks "..", "." and doubled separators before a rule ever sees them. This is
// the same test, with the trailing slash preserved the way Node's
// path.normalize preserves it.
func isNormalizedPath(p string) bool {
	if p == "" {
		return false
	}
	cleaned := path.Clean(p)
	if strings.HasSuffix(p, "/") && p != "/" {
		cleaned += "/"
	}
	return cleaned == p
}
