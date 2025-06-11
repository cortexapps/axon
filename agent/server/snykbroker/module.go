package snykbroker

import (
	"go.uber.org/fx"
)

var Module = fx.Module("snykbroker",
	fx.Provide(NewRegistration),
	fx.Invoke(NewRelayInstanceManager),
)
