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

func MaybeNewRegistrationReflector(config config.AgentConfig, p RegistrationReflectorParams) *RegistrationReflector {

	if !config.EnableHttpRelayReflector {
		return nil
	}

	return NewRegistrationReflector(p)
}
