package requestexecutor

import (
	"path"
	"strings"

	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
)

// MatchRule finds the first accept file rule that matches the given HTTP method and path.
// Returns nil if no rule matches.
func MatchRule(rules []acceptfile.AcceptFileRuleWrapper, method, requestPath string) *acceptfile.AcceptFileRuleWrapper {
	for i := range rules {
		rule := &rules[i]
		if matchesMethod(rule.Method(), method) && matchesPath(rule.Path(), requestPath) {
			return rule
		}
	}
	return nil
}

// matchesMethod checks if the rule method matches the request method.
// "any" matches all methods.
func matchesMethod(ruleMethod, requestMethod string) bool {
	if strings.EqualFold(ruleMethod, "any") {
		return true
	}
	return strings.EqualFold(ruleMethod, requestMethod)
}

// matchesPath checks if the request path matches the rule's path pattern.
// Supports glob-style wildcards: * matches a single path segment, ** matches any number.
func matchesPath(pattern, requestPath string) bool {
	if pattern == "" {
		return false
	}

	// Normalize paths.
	pattern = "/" + strings.TrimLeft(pattern, "/")
	requestPath = "/" + strings.TrimLeft(requestPath, "/")

	// Use path.Match for simple glob patterns.
	// Handle trailing /* as "match anything under this prefix".
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		if strings.HasPrefix(requestPath, prefix+"/") || requestPath == prefix {
			return true
		}
	}

	// Try exact path.Match.
	matched, err := path.Match(pattern, requestPath)
	if err != nil {
		return false
	}
	return matched
}
