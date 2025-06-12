package util

import (
	"os"
	"strings"
)

func hasProxy() bool {
	proxy := os.Getenv("HTTP_PROXY")
	if proxy == "" {
		proxy = os.Getenv("HTTPS_PROXY")
	}
	return proxy != ""
}

func EnsureLocalhostNoProxy(updateEnv bool) string {
	// ensure no proxy has localhost
	noProxy := os.Getenv("NO_PROXY")
	if hasProxy() {
		settings := strings.Split(noProxy, ",")
		if !strings.Contains(noProxy, "localhost") {
			settings = append(settings, "localhost")
		}
		if !strings.Contains(noProxy, "127.0.0.1") {
			settings = append(settings, "127.0.0.1")
		}
		newNoProxy := strings.Trim(strings.Join(settings, ","), " ,")
		if updateEnv {
			os.Setenv("NO_PROXY", newNoProxy)
		}
		noProxy = newNoProxy
	}
	return noProxy
}
