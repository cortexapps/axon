package snykbroker

import (
	"net/http"

	"github.com/cortexapps/axon/config"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

var Module = fx.Module("snykbroker",
	fx.Provide(NewRegistration),
	fx.Provide(MaybeNewRegistrationReflector),
	fx.Invoke(NewRelayInstanceManager),
)

func MaybeNewRegistrationReflector(lifecycle fx.Lifecycle, config config.AgentConfig, logger *zap.Logger, transport *http.Transport) *RegistrationReflector {

	if !config.EnableHttpRelayReflector {
		return nil
	}

	return NewRegistrationReflector(lifecycle, logger, transport)
}
