package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildVersionFromEnv(t *testing.T) {
	// The release image sets AXON_BUILD_VERSION; a server built from a tag
	// must report that stamp rather than a placeholder, otherwise there is no
	// way to tell which release a pod is running.
	t.Run("stamped build reports its version", func(t *testing.T) {
		t.Setenv("AXON_BUILD_VERSION", "v0.2.12-abc1234")
		cfg := NewConfigFromEnv()
		require.Equal(t, "v0.2.12-abc1234", cfg.BuildVersion)
	})

	// Unset and empty both mean "nobody stamped this", and both must land on
	// "dev" — an empty string would render as a blank field in the startup log
	// and the health response, which reads like a bug rather than a local run.
	t.Run("unstamped build reports dev", func(t *testing.T) {
		t.Setenv("AXON_BUILD_VERSION", "")
		require.Equal(t, DefaultBuildVersion, NewConfigFromEnv().BuildVersion)
	})
}

func TestBuildVersionIsLogged(t *testing.T) {
	// The startup config line is where an operator looks first, so the version
	// has to be in it.
	t.Setenv("AXON_BUILD_VERSION", "v0.2.12-abc1234")
	cfg := NewConfigFromEnv()

	var found bool
	for _, f := range cfg.Fields() {
		if f.Key == "build_version" {
			found = true
			require.Equal(t, "v0.2.12-abc1234", f.String)
		}
	}
	require.True(t, found, "build_version missing from Config.Fields()")
}
