package config

import (
	"os"
	"testing"
	"time"

	"github.com/cortexapps/axon/util"
	"github.com/stretchr/testify/require"
)

func resetEnv() {
	varsToClear := []string{
		"CORTEX_API_BASE_URL",
		"CORTEX_API_TOKEN",
		"PORT",
		"HTTP_PORT",
		"SNYK_BROKER_PORT",
		"DRYRUN",
		"DEQUEUE_WAIT_TIME",
		"CA_CERT_PATH",
		"DISABLE_TLS",
		"ENABLE_RELAY_REFLECTOR",
		"REFLECTOR_WEBSOCKET_UPGRADE",
	}

	for _, v := range varsToClear {
		if err := os.Unsetenv(v); err != nil {
			panic(err)
		}
	}
}

func TestNewAgentEnvConfig_DefaultValues(t *testing.T) {
	oldEnv := util.SaveEnv(false)
	defer util.RestoreEnv(oldEnv)
	resetEnv()

	config := NewAgentEnvConfig()

	require.Equal(t, 50051, config.GrpcPort)
	require.Equal(t, "https://api.getcortexapp.com", config.CortexApiBaseUrl)
	require.Equal(t, "", config.CortexApiToken)
	require.False(t, config.DryRun)
	require.Equal(t, 1*time.Second, config.DequeueWaitTime)
	// Default reflector mode should be TrafficOnly (reflects traffic but not registration)
	require.Equal(t, RelayReflectorTrafficOnly, config.HttpRelayReflectorMode)
	require.True(t, config.HttpRelayReflectorMode.ReflectsTraffic())
	require.False(t, config.HttpRelayReflectorMode.ReflectsRegistration())
	// WebSocket upgrade should be enabled by default
	require.True(t, config.ReflectorWebSocketUpgrade)
}

func TestNewAgentEnvConfig_CustomValues(t *testing.T) {
	resetEnv()
	os.Setenv("CORTEX_API_BASE_URL", "https://custom.api.url")
	os.Setenv("PORT", "12345")
	os.Setenv("CORTEX_API_TOKEN", "custom_token")
	os.Setenv("DEQUEUE_WAIT_TIME", "2s")

	config := NewAgentEnvConfig()

	require.Equal(t, 12345, config.GrpcPort)
	require.Equal(t, "https://custom.api.url", config.CortexApiBaseUrl)
	require.Equal(t, "custom_token", config.CortexApiToken)
	require.False(t, config.DryRun)
	require.Equal(t, 2*time.Second, config.DequeueWaitTime)
}

func TestNewAgentEnvConfig_CustomValues_DRYRUN(t *testing.T) {
	resetEnv()
	os.Setenv("CORTEX_API_TOKEN", "custom_token")
	os.Setenv("DRYRUN", "true")

	config := NewAgentEnvConfig()

	require.Equal(t, "dry-run", config.CortexApiToken)
	require.True(t, config.DryRun)
}

func TestNewAgentEnvConfig_InvalidPort(t *testing.T) {
	resetEnv()
	os.Setenv("PORT", "invalid")

	defer func() {
		if r := recover(); r == nil {
			t.Errorf("Expected panic for invalid port")
		}
	}()

	NewAgentEnvConfig()
}

func TestNewAgentEnvConfig_InvalidDequeueWaitTime(t *testing.T) {
	resetEnv()
	os.Setenv("DEQUEUE_WAIT_TIME", "invalid")

	defer func() {
		if r := recover(); r == nil {
			t.Errorf("Expected panic for invalid dequeue wait time")
		}
	}()

	NewAgentEnvConfig()
}

func TestLoadCaCertsDir(t *testing.T) {
	oldEnv := util.SaveEnv(false)
	defer util.RestoreEnv(oldEnv)
	resetEnv()
	os.Setenv("CA_CERT_PATH", "/tmp/foo/../bar/certs/cert.pem")
	os.Setenv("DISABLE_TLS", "true")

	config := NewAgentEnvConfig()
	require.Equal(t, true, config.HttpDisableTLS)
	require.Equal(t, "/tmp/bar/certs/cert.pem", config.HttpCaCertFilePath)
}

func TestEnableRelayReflectorEnvVar(t *testing.T) {
	tests := []struct {
		envValue     string
		expectedMode RelayReflectorMode
	}{
		{"disabled", RelayReflectorDisabled},
		{"false", RelayReflectorDisabled},
		{"registration", RelayReflectorRegistrationOnly},
		{"traffic", RelayReflectorTrafficOnly},
		{"all", RelayReflectorAllTraffic},
		{"true", RelayReflectorAllTraffic},
	}

	for _, tt := range tests {
		t.Run(tt.envValue, func(t *testing.T) {
			oldEnv := util.SaveEnv(false)
			defer util.RestoreEnv(oldEnv)
			resetEnv()
			os.Setenv("ENABLE_RELAY_REFLECTOR", tt.envValue)

			config := NewAgentEnvConfig()
			require.Equal(t, tt.expectedMode, config.HttpRelayReflectorMode)
		})
	}
}

func TestReflectorWebSocketUpgradeEnvVar(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		expected bool
	}{
		{"default (not set)", "", true},
		{"explicitly true", "true", true},
		{"explicitly false", "false", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldEnv := util.SaveEnv(false)
			defer util.RestoreEnv(oldEnv)
			resetEnv()
			if tt.envValue != "" {
				os.Setenv("REFLECTOR_WEBSOCKET_UPGRADE", tt.envValue)
			}

			config := NewAgentEnvConfig()
			require.Equal(t, tt.expected, config.ReflectorWebSocketUpgrade)
		})
	}
}

func TestRelayReflectorMode_Helpers(t *testing.T) {
	tests := []struct {
		name                 string
		mode                 RelayReflectorMode
		isEnabled            bool
		reflectsRegistration bool
		reflectsTraffic      bool
	}{
		{
			name:                 "Disabled",
			mode:                 RelayReflectorDisabled,
			isEnabled:            false,
			reflectsRegistration: false,
			reflectsTraffic:      false,
		},
		{
			name:                 "RegistrationOnly",
			mode:                 RelayReflectorRegistrationOnly,
			isEnabled:            true,
			reflectsRegistration: true,
			reflectsTraffic:      false,
		},
		{
			name:                 "TrafficOnly",
			mode:                 RelayReflectorTrafficOnly,
			isEnabled:            true,
			reflectsRegistration: false,
			reflectsTraffic:      true,
		},
		{
			name:                 "AllTraffic",
			mode:                 RelayReflectorAllTraffic,
			isEnabled:            true,
			reflectsRegistration: true,
			reflectsTraffic:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.isEnabled, tt.mode.IsEnabled(), "IsEnabled")
			require.Equal(t, tt.reflectsRegistration, tt.mode.ReflectsRegistration(), "ReflectsRegistration")
			require.Equal(t, tt.reflectsTraffic, tt.mode.ReflectsTraffic(), "ReflectsTraffic")
		})
	}
}
