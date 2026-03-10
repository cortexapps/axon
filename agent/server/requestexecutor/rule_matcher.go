package requestexecutor

import (
	"path"
	"strings"

	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
)

// MatchRule finds the first accept file rule that matches the given HTTP method, path, and headers.
// Returns nil if no rule matches.
func MatchRule(rules []acceptfile.AcceptFileRuleWrapper, method, requestPath string, headers ...map[string]string) *acceptfile.AcceptFileRuleWrapper {
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
func matchesValid(requirements []acceptfile.ValidHeaderRequirement, headers map[string]string) bool {
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

// getHeaderCaseInsensitive retrieves a header value with case-insensitive key matching.
func getHeaderCaseInsensitive(headers map[string]string, key string) (string, bool) {
	if headers == nil {
		return "", false
	}

	// Try exact match first.
	if v, ok := headers[key]; ok {
		return v, true
	}

	// Case-insensitive search.
	keyLower := strings.ToLower(key)
	for k, v := range headers {
		if strings.ToLower(k) == keyLower {
			return v, true
		}
	}

	return "", false
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
