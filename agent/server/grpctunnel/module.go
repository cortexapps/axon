package grpctunnel

import (
	"github.com/cortexapps/axon/server/snykbroker"
	"go.uber.org/fx"
)

var Module = fx.Module("grpctunnel",
	fx.Provide(snykbroker.NewRegistration),
	fx.Invoke(NewTunnelClient),
)
