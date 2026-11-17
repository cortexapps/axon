package snykbroker

import (
	"github.com/cortexapps/axon/config"
)

func MaybeNewRegistrationReflector(cfg config.AgentConfig, p RegistrationReflectorParams) *RegistrationReflector {

	if !cfg.HttpRelayReflectorMode.IsEnabled() {
		return nil
	}

	return NewRegistrationReflector(p)
}
