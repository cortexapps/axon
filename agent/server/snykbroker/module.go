package snykbroker

import (
	"github.com/cortexapps/axon/config"
	"go.uber.org/fx"
)

var Module = fx.Module("snykbroker",
	fx.Provide(NewRegistration),
	fx.Provide(MaybeNewRegistrationReflector),
	fx.Invoke(NewRelayInstanceManager),
)

func MaybeNewRegistrationReflector(cfg config.AgentConfig, p RegistrationReflectorParams) *RegistrationReflector {

	if cfg.HttpRelayReflectorMode != config.RelayReflectorDisabled {
		return nil
	}

	return NewRegistrationReflector(p)
}
