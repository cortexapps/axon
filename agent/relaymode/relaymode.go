// Package relaymode decides which relay tunnel the agent runs, and records
// the tunnel that runs now.
//
// Precedence, highest first:
//
//  1. The agent configuration: AXON_RELAY_TRANSPORT (or RELAY_MODE), or the
//     mode a build forces (config.ForcedRelayMode). This pins the mode, and
//     the agent never changes it at runtime.
//  2. The tunnelMode field of the Cortex registration response. Cortex sets
//     it per tenant, and the agent follows a change at runtime.
//  3. Default.
package relaymode

import (
	"sync/atomic"

	"github.com/cortexapps/axon/config"
)

// Default is the mode the agent runs when neither the configuration nor
// Cortex selects one. It is here and not in config so that the
// configuration can tell a customer choice from the default.
const Default = config.RelayModeSnykBroker

// Parse returns the mode that s names, and false if s names no known mode.
func Parse(s string) (config.RelayMode, bool) {
	switch m := config.RelayMode(s); m {
	case config.RelayModeSnykBroker, config.RelayModeGrpcTunnel:
		return m, true
	}
	return "", false
}

// Pinned returns the mode that the configuration selects, and false if the
// configuration selects none. Any configured value pins the mode. Only an
// exact "grpc-tunnel" selects the tunnel, and all other values run
// snyk-broker, as they always did.
func Pinned(cfg config.AgentConfig) (config.RelayMode, bool) {
	if !cfg.RelayModePinned() {
		return "", false
	}
	if cfg.IsGRPCTunnel() {
		return config.RelayModeGrpcTunnel, true
	}
	return config.RelayModeSnykBroker, true
}

// FromRegistration returns the mode for the tunnelMode value of a
// registration response. Empty means that Cortex has no preference, so the
// result is Default. The second result is false if the value is not empty
// and names no known mode; the result is then also Default.
func FromRegistration(tunnelMode string) (config.RelayMode, bool) {
	if tunnelMode == "" {
		return Default, true
	}
	if m, ok := Parse(tunnelMode); ok {
		return m, true
	}
	return Default, false
}

// Effective returns the mode for an agent that has no Status to ask: the
// pinned mode, or Default.
func Effective(cfg config.AgentConfig) config.RelayMode {
	if m, ok := Pinned(cfg); ok {
		return m
	}
	return Default
}

// Status records the mode that runs now. It is safe for concurrent use.
type Status struct {
	active atomic.Value // config.RelayMode
}

func NewStatus() *Status {
	return &Status{}
}

// Active returns the mode that runs now, or "" before a tunnel starts.
func (s *Status) Active() config.RelayMode {
	if m, ok := s.active.Load().(config.RelayMode); ok {
		return m
	}
	return ""
}

// Set records the mode that runs now.
func (s *Status) Set(m config.RelayMode) {
	s.active.Store(m)
}
