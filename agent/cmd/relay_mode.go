package cmd

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/server/relaymode"
	"github.com/cortexapps/axon/server/snykbroker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// defaultRelayModeCheckInterval applies when AUTO_REGISTER_FREQUENCY is 0.
const defaultRelayModeCheckInterval = 5 * time.Minute

// restartRequested is set when the relay mode watcher stops the app because
// Cortex selected a different mode. startAgent then starts the agent again.
var restartRequested atomic.Bool

// withRelayMode returns cfg with RelayMode set to the mode the stack runs,
// and an fx option that watches Cortex for a different mode. It runs
// before the stack is built, because the mode decides which relay module
// the stack contains. The registration uses the same transport settings
// (proxy, CA) as the stack.
func withRelayMode(cfg config.AgentConfig, info common.IntegrationInfo) (config.AgentConfig, fx.Option) {
	reg := snykbroker.NewRegistration(cfg, createHttpClient(cfg, createHttpTransport(cfg, zap.NewNop())))
	res := relaymode.Resolve(context.Background(), cfg, info, reg)
	fmt.Printf("Relay mode: %s, %s\n", res.Mode, res.Reason)
	cfg.RelayMode = string(res.Mode)

	if !res.Remote {
		return cfg, fx.Options()
	}
	return cfg, fx.Invoke(func(lc fx.Lifecycle, shutdowner fx.Shutdowner, logger *zap.Logger, reg snykbroker.Registration) {
		watchRelayMode(lc, shutdowner, logger, cfg, info, reg, res.Mode)
	})
}

// watchRelayMode runs relaymode.Watch for the life of the app. When Cortex
// selects a different mode, it stops the app and requests a restart.
func watchRelayMode(
	lc fx.Lifecycle,
	shutdowner fx.Shutdowner,
	logger *zap.Logger,
	cfg config.AgentConfig,
	info common.IntegrationInfo,
	reg snykbroker.Registration,
	running config.RelayMode,
) {
	interval := cfg.AutoRegisterFrequency
	if interval <= 0 {
		interval = defaultRelayModeCheckInterval
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() {
				defer close(done)
				relaymode.Watch(ctx, interval, info, reg, running, func(mode config.RelayMode) {
					logger.Info("Cortex selected a different relay mode, restarting the agent",
						zap.String("from", string(running)), zap.String("to", string(mode)))
					restartRequested.Store(true)
					if err := shutdowner.Shutdown(); err != nil {
						logger.Error("Unable to stop the agent for a relay mode change", zap.Error(err))
					}
				})
			}()
			return nil
		},
		OnStop: func(context.Context) error {
			cancel()
			<-done
			return nil
		},
	})
}

// reexec replaces this process with a new run of the same binary, with the
// same arguments and environment. The PID stays the same, so it works with
// no restart policy, and a container does not restart. The app must be
// stopped first, so that child processes (snyk-broker) are gone and ports
// are free. If the exec fails, the agent exits so that a restart policy can
// start it again.
func reexec() {
	exe, err := os.Executable()
	if err == nil {
		fmt.Println("Starting the agent again with the new relay mode")
		err = syscall.Exec(exe, os.Args, os.Environ())
	}
	fmt.Printf("Unable to start the agent again: %v\n", err)
	os.Exit(1)
}
