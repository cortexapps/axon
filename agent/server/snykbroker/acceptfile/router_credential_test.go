package acceptfile

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A credential provider that fails has to refuse the request, not let its
// placeholder travel upstream as the credential — the upstream then refuses on
// authorization and the log names the wrong culprit. #127 made ResolverMap
// report that failure instead of swallowing it, and taught the reflector's
// serve() to answer 502. This pins the same behaviour on the tunnel path,
// which reads the same ResolverMap through Router.Route.
//
// Route returns a plain error here rather than a classified one, which is what
// puts it in the default arm of grpctunnel's RouteError mapping — a 502, the
// same status the reflector sends.
func TestRouteRefusesWhenACredentialProviderFails(t *testing.T) {
	router := newTestRouter(t, `{"private":[{
		"method": "any",
		"path": "/*",
		"origin": "https://api.example",
		"headers": {"authorization": "${plugin:plugin_fail.sh}"}
	}]}`, ".")

	_, err := router.Route("GET", "/x", nil)
	require.Error(t, err, "a failing credential provider must fail the request")
	require.Contains(t, err.Error(), "credential provider failed")
	require.NotErrorIs(t, err, ErrNoRoute, "the rule matched; it was the credential that failed")
}

// The companion case: a provider that succeeds still reaches the upstream, so
// the check above is refusing on the failure rather than on having a plugin.
func TestRouteCarriesAResolvedPluginCredential(t *testing.T) {
	router := newTestRouter(t, `{"private":[{
		"method": "any",
		"path": "/*",
		"origin": "https://api.example",
		"headers": {"x-plugin-output": "${plugin:plugin.sh}"}
	}]}`, ".")

	req, err := router.Route("GET", "/x", nil)
	require.NoError(t, err)
	require.NotEmpty(t, req.Header.Get("x-plugin-output"))
}
