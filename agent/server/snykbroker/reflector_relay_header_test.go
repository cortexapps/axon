package snykbroker

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// The routing header must be removed on every path, not only on the entries
// that consume it. A concrete origin has no use for the header, so a value
// that reaches its upstream is a value the operator never authorized sending
// there.
//
// The boundary cannot rest on the upstream declining to echo what it receives.
// A controlled-account check found one API family that reflects arbitrary
// request header names into a CORS preflight response. Bounded on re-testing -
// names only, values never returned - so it does not expose this header's
// value, but it does settle where the control belongs. Removal on the agent
// side is the control, and this pins it.
//
// The value has to agree with the declared origin, or the request is rejected
// before it is forwarded and nothing is proven about removal. That is also why
// the origin names "localhost" rather than httptest's 127.0.0.1: an IP literal
// is never an accepted authority.
func TestRelayTargetHostHeaderNeverReachesUpstream(t *testing.T) {
	for _, tc := range []struct {
		name      string
		isDefault bool
	}{
		{name: "keyed concrete origin", isDefault: false},
		{name: "default origin", isDefault: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestReflectorEnv(t)
			backend := newRecordingBackend(t, "reached")

			parsed, err := url.Parse(backend.server.URL)
			require.NoError(t, err)
			origin := "http://localhost:" + parsed.Port()

			proxyURI := env.Reflector.ProxyURI(origin,
				WithDefault(tc.isDefault),
				WithHeaders(map[string]string{"Authorization": "Bearer minted"}),
			)

			req := httptest.NewRequest("GET", proxyPath(t, proxyURI)+"/v1/things", nil)
			req.Header.Set(HeaderTargetHost, "localhost")

			rec := httptest.NewRecorder()
			env.Reflector.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)

			hits, _, upstream := backend.snapshot()
			require.Equal(t, 1, hits)
			require.Equal(t, "Bearer minted", upstream.Get("Authorization"),
				"the rule header is still injected")
			require.Empty(t, upstream.Values(HeaderTargetHost),
				"the routing header reached the upstream origin")
		})
	}
}
