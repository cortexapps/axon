package relay

import (
	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/relaymode"
	"github.com/cortexapps/axon/server/snykbroker"
	"go.uber.org/fx"
)

// Module runs the relay tunnel through a Switcher, which selects the mode by
// the relaymode precedence and can change it at runtime.
var Module = fx.Module("relay",
	fx.Provide(snykbroker.NewRegistration),
	fx.Provide(newReflector),
	fx.Provide(relaymode.NewStatus),
	fx.Provide(NewSwitcher),
	fx.Invoke(func(*Switcher) {}),
)

// newReflector builds the snyk-broker registration reflector, unless the
// configuration pins a mode that does not use it.
func newReflector(cfg config.AgentConfig, p snykbroker.RegistrationReflectorParams) *snykbroker.RegistrationReflector {
	if m, ok := relaymode.Pinned(cfg); ok && m != config.RelayModeSnykBroker {
		return nil
	}
	return snykbroker.MaybeNewRegistrationReflector(cfg, p)
}
