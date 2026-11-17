// Package relaymode selects the relay tunnel mode once, before the agent
// builds its stack. It has no fx wiring, so it can be called on its own.
//
// Precedence, highest first:
//
//  1. The agent configuration: AXON_RELAY_TRANSPORT (or RELAY_MODE), or the
//     mode a build forces (config.ForcedRelayMode).
//  2. The tunnelMode field of the Cortex registration response.
//  3. Default.
//
// A change in Cortex takes effect when the agent starts again.
package relaymode

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/snykbroker"
)

// Default is the mode the agent runs when neither the configuration nor
// Cortex selects one. It is here and not in config so that the
// configuration can tell a customer choice from the default.
const Default = config.RelayModeSnykBroker

// registerTimeout bounds the registration call, so that a slow Cortex API
// delays the agent start by a known amount at most. A package variable so
// tests can shorten it.
var registerTimeout = 10 * time.Second

// Resolve returns the mode to run, and a reason for the log. It calls
// Register only when the configuration does not select a mode. If the call
// fails, or Cortex has no preference or sends an unknown value, the result
// is Default.
func Resolve(ctx context.Context, cfg config.AgentConfig, info common.IntegrationInfo, reg snykbroker.Registration) (config.RelayMode, string) {
	if cfg.RelayModePinned() {
		// Only an exact "grpc-tunnel" selects the tunnel. All other values
		// run snyk-broker, as they always did.
		if cfg.IsGRPCTunnel() {
			return config.RelayModeGrpcTunnel, "set by configuration"
		}
		return config.RelayModeSnykBroker, "set by configuration"
	}

	// The tunnels do not register in these cases, so neither do we.
	if cfg.DryRun {
		return Default, "default (dry run)"
	}
	if os.Getenv("BROKER_SERVER_URL") != "" && os.Getenv("BROKER_TOKEN") != "" {
		return Default, "default (BROKER_SERVER_URL and BROKER_TOKEN set)"
	}

	ctx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()
	resp, err := reg.Register(ctx, info.Integration, info.Alias)
	if err != nil {
		return Default, fmt.Sprintf("default (registration failed: %v)", err)
	}

	switch m := config.RelayMode(resp.TunnelMode); m {
	case "":
		return Default, "default (Cortex has no preference)"
	case config.RelayModeSnykBroker, config.RelayModeGrpcTunnel:
		return m, "selected by Cortex"
	default:
		return Default, fmt.Sprintf("default (unknown tunnelMode %q from Cortex)", resp.TunnelMode)
	}
}
