package acceptfile

import "strings"

// Request-header helpers shared by rule matching and routing. None of this is
// origin policy or match semantics; it is just the handful of things that have
// to be true when headers arrive as a map with whatever casing the caller used.

// getHeaderCaseInsensitive retrieves a header value with case-insensitive key
// matching.
func getHeaderCaseInsensitive(headers map[string]string, key string) (string, bool) {
	if headers == nil {
		return "", false
	}

	// Try exact match first.
	if v, ok := headers[key]; ok {
		return v, true
	}

	keyLower := strings.ToLower(key)
	for k, v := range headers {
		if strings.ToLower(k) == keyLower {
			return v, true
		}
	}

	return "", false
}

// isWebSocketUpgrade reports whether the request headers ask for an upgrade.
func isWebSocketUpgrade(headers map[string]string) bool {
	upgrade, ok := getHeaderCaseInsensitive(headers, "Upgrade")
	if !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(upgrade), "websocket")
}

// takeTargetHosts removes every spelling of the target-host header, returning
// the values it carried and the headers without it. Callers get a copy, so the
// caller's own map is left alone.
//
// Every spelling, and all of them: a map can hold the header twice under
// different casing, and two values is a shape the destination policy refuses
// rather than picks from.
func takeTargetHosts(headers map[string]string) ([]string, map[string]string) {
	var values []string
	remaining := make(map[string]string, len(headers))
	for k, v := range headers {
		if strings.EqualFold(k, HeaderTargetHost) {
			values = append(values, v)
			continue
		}
		remaining[k] = v
	}
	return values, remaining
}
