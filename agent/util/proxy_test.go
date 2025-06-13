package util

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnsureLocalhostNoProxy_NoProxySet(t *testing.T) {
	os.Setenv("HTTP_PROXY", "")
	os.Setenv("HTTPS_PROXY", "")
	os.Setenv("NO_PROXY", "")
	result := EnsureLocalhostNoProxy(true)
	require.Equal(t, "", result)
	require.Equal(t, "", os.Getenv("NO_PROXY"))
}

func TestEnsureLocalhostNoProxy_AddsLocalhost(t *testing.T) {
	os.Setenv("HTTP_PROXY", "http://proxy")
	os.Setenv("NO_PROXY", "")
	result := EnsureLocalhostNoProxy(true)
	require.Contains(t, os.Getenv("NO_PROXY"), "localhost")
	require.Contains(t, os.Getenv("NO_PROXY"), "127.0.0.1")
	require.Equal(t, "localhost,127.0.0.1", result)
}

func TestEnsureLocalhostNoProxy_AlreadyPresent(t *testing.T) {
	os.Setenv("HTTP_PROXY", "http://proxy")
	os.Setenv("NO_PROXY", "localhost,127.0.0.1,example.com")
	result := EnsureLocalhostNoProxy(true)
	require.Equal(t, "localhost,127.0.0.1,example.com", os.Getenv("NO_PROXY"))
	require.Equal(t, "localhost,127.0.0.1,example.com", result)
}

func TestEnsureLocalhostNoProxy_PartialPresent(t *testing.T) {
	os.Setenv("HTTP_PROXY", "http://proxy")
	os.Setenv("NO_PROXY", "example.com")
	result := EnsureLocalhostNoProxy(true)
	noProxy := os.Getenv("NO_PROXY")
	require.Contains(t, noProxy, "localhost")
	require.Contains(t, noProxy, "127.0.0.1")
	require.Contains(t, noProxy, "example.com")
	require.Equal(t, "example.com,localhost,127.0.0.1", result)
}
