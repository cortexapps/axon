// Package relaymode selects the relay tunnel mode before the agent builds
// its stack, and watches Cortex for a change after that. It has no fx
// wiring, so it can be called on its own.
//
// Precedence, highest first:
//
//  1. The agent configuration: AXON_RELAY_TRANSPORT (or RELAY_MODE), or the
//     mode a build forces (config.ForcedRelayMode).
//  2. The tunnelMode field of the Cortex registration response.
//  3. Default.
//
// When the configuration does not select a mode, the mode is remote-driven:
// Resolve makes a best-effort registration at start, and Watch registers
// again at an interval. When a successful registration selects a different
// mode than the one that runs, the agent starts again with the new mode.
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

// registerTimeout bounds each registration call, so that a slow Cortex API
// delays the agent start by a known amount at most. A package variable so
// tests can shorten it.
var registerTimeout = 10 * time.Second

// Result is the mode Resolve selected.
type Result struct {
	Mode config.RelayMode
	// Reason tells where Mode came from, for the log.
	Reason string
	// Remote is true when Cortex drives the mode, so Watch must run. It is
	// true also when the registration at start failed and Mode is Default.
	Remote bool
}

// Resolve returns the mode to run. It calls Register only when the
// configuration does not select a mode. If the call fails, or Cortex sends
// no value or an unknown value, the mode is Default.
func Resolve(ctx context.Context, cfg config.AgentConfig, info common.IntegrationInfo, reg snykbroker.Registration) Result {
	if cfg.RelayModePinned() {
		// Only an exact "grpc-tunnel" selects the tunnel. All other values
		// run snyk-broker, as they always did.
		if cfg.IsGRPCTunnel() {
			return Result{Mode: config.RelayModeGrpcTunnel, Reason: "set by configuration"}
		}
		return Result{Mode: config.RelayModeSnykBroker, Reason: "set by configuration"}
	}

	// The tunnels do not register in these cases, so neither do we.
	if cfg.DryRun {
		return Result{Mode: Default, Reason: "default (dry run)"}
	}
	if os.Getenv("BROKER_SERVER_URL") != "" && os.Getenv("BROKER_TOKEN") != "" {
		return Result{Mode: Default, Reason: "default (BROKER_SERVER_URL and BROKER_TOKEN set)"}
	}

	resp, err := register(ctx, info, reg)
	if err != nil {
		return Result{Mode: Default, Reason: fmt.Sprintf("default (registration failed: %v)", err), Remote: true}
	}
	mode, known := FromRegistration(resp)
	switch {
	case !known:
		return Result{Mode: Default, Reason: fmt.Sprintf("default (unknown tunnelMode %q from Cortex)", resp.TunnelMode), Remote: true}
	case resp.TunnelMode == "":
		return Result{Mode: Default, Reason: "default (Cortex has no preference)", Remote: true}
	default:
		return Result{Mode: mode, Reason: "selected by Cortex", Remote: true}
	}
}

// FromRegistration returns the mode that a registration response selects.
// No tunnelMode selects Default. The second result is false when the
// tunnelMode is not a known mode.
func FromRegistration(resp *snykbroker.RegistrationInfoResponse) (config.RelayMode, bool) {
	switch m := config.RelayMode(resp.TunnelMode); m {
	case "":
		return Default, true
	case config.RelayModeSnykBroker, config.RelayModeGrpcTunnel:
		return m, true
	}
	return Default, false
}

// Watch registers at each interval until ctx ends. When a successful
// registration selects a different mode than running, it calls onChange
// with that mode and returns. A failed registration, or a tunnelMode that
// is not a known mode, changes nothing.
func Watch(ctx context.Context, interval time.Duration, info common.IntegrationInfo, reg snykbroker.Registration, running config.RelayMode, onChange func(config.RelayMode)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		resp, err := register(ctx, info, reg)
		if err != nil {
			continue
		}
		if mode, known := FromRegistration(resp); known && mode != running {
			onChange(mode)
			return
		}
	}
}

func register(ctx context.Context, info common.IntegrationInfo, reg snykbroker.Registration) (*snykbroker.RegistrationInfoResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()
	return reg.Register(ctx, info.Integration, info.Alias)
}
