package acceptfile

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Auth parity with snyk-broker's lib/common/utils/auth-header.ts. Axon accept
// files ship only bearer and basic today, but a customer-authored file may use
// any of these and "drop-in replacement" has to mean it.

func authRules(t *testing.T, authJSON string) *Router {
	t.Helper()
	t.Setenv("UPSTREAM", "https://up.example")
	return newTestRouter(t, fmt.Sprintf(
		`{"private":[{"method":"any","path":"/*","origin":"${UPSTREAM}","auth":%s}]}`, authJSON))
}

func authHeaderFor(t *testing.T, authJSON string) string {
	t.Helper()
	routed, err := authRules(t, authJSON).Route("GET", "/x", nil)
	require.NoError(t, err)
	return routed.Header.Get("Authorization")
}

func TestAuthBearer(t *testing.T) {
	t.Setenv("TOK", "abc")
	require.Equal(t, "Bearer abc", authHeaderFor(t, `{"scheme":"bearer","token":"${TOK}"}`))
}

// The broker emits "Token", not "Bearer". They are different schemes and some
// upstreams accept only one.
func TestAuthTokenSchemeEmitsToken(t *testing.T) {
	t.Setenv("TOK", "abc")
	require.Equal(t, "Token abc", authHeaderFor(t, `{"scheme":"token","token":"${TOK}"}`))
}

// "raw" is the escape hatch for an upstream whose header carries no scheme
// prefix at all.
func TestAuthRawSchemeEmitsTheTokenVerbatim(t *testing.T) {
	t.Setenv("TOK", "abc-123")
	require.Equal(t, "abc-123", authHeaderFor(t, `{"scheme":"raw","token":"${TOK}"}`))
}

func TestAuthBasicFromUsernameAndPassword(t *testing.T) {
	t.Setenv("USER", "svc-user")
	t.Setenv("PASS", "svc-pass")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("svc-user:svc-pass"))
	require.Equal(t, want,
		authHeaderFor(t, `{"scheme":"basic","username":"${USER}","password":"${PASS}"}`))
}

// A basic block carrying "token" holds the already-encoded user:pass pair (the
// shape Azure Repos uses). Encoding it a second time sends a broken credential,
// and dropping it sends an empty one.
func TestAuthBasicWithPreEncodedToken(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("user:pass"))
	t.Setenv("PRE_ENCODED", encoded)
	require.Equal(t, "Basic "+encoded,
		authHeaderFor(t, `{"scheme":"basic","token":"${PRE_ENCODED}"}`))
}

func TestAuthBasicPrefersUsernamePasswordWhenBothPresent(t *testing.T) {
	t.Setenv("USER", "u")
	t.Setenv("PASS", "p")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))
	require.Equal(t, want,
		authHeaderFor(t, `{"scheme":"basic","username":"${USER}","password":"${PASS}","token":"ignored"}`))
}

func TestAuthSchemeIsCaseInsensitive(t *testing.T) {
	t.Setenv("TOK", "abc")
	require.Equal(t, "Bearer abc", authHeaderFor(t, `{"scheme":"Bearer","token":"${TOK}"}`))
	require.Equal(t, "Token abc", authHeaderFor(t, `{"scheme":"TOKEN","token":"${TOK}"}`))
}

// An unrecognized scheme used to become "<scheme> <token>" — a credential in a
// shape no upstream asked for. snyk-broker sends no header at all for one, so
// that is what the Router does; the warning is covered in
// TestUnknownAuthSchemeWarnsAndSendsNoHeader.
func TestUnknownAuthSchemeSendsNoHeader(t *testing.T) {
	t.Setenv("TOK", "abc")
	require.Empty(t, authHeaderFor(t, `{"scheme":"digest","token":"${TOK}"}`))
}

// The rule owns the credential; a caller cannot substitute its own.
func TestRuleAuthOverridesCallerAuthorization(t *testing.T) {
	t.Setenv("TOK", "rule-token")
	routed, err := authRules(t, `{"scheme":"bearer","token":"${TOK}"}`).
		Route("GET", "/x", map[string]string{"Authorization": "Bearer caller-token"})
	require.NoError(t, err)
	require.Equal(t, "Bearer rule-token", routed.Header.Get("Authorization"))
}

// Every scheme the table declares must actually produce a credential, and
// nothing outside the table may produce one. This is what keeps "supported"
// meaning the same thing in both places it is consulted: the warning at Router
// construction, and applyAuth on the request.
//
// Before authHeaderBuilders these were two lists — a set in supported.go and a
// switch in router.go — that could disagree without anything noticing.
func TestEveryDeclaredAuthSchemeBuildsACredential(t *testing.T) {
	require.NotEmpty(t, authHeaderBuilders)

	authBlock := func(scheme authScheme) string {
		return `{"scheme":"` + string(scheme) +
			`","token":"${TOK}","username":"${USER}","password":"${PASS}"}`
	}

	for scheme := range authHeaderBuilders {
		t.Run(string(scheme), func(t *testing.T) {
			require.True(t, isSupportedAuthScheme(string(scheme)),
				"a declared scheme must report as supported")
			require.Contains(t, supportedAuthSchemes(), string(scheme),
				"a declared scheme must be named in the warning")

			// Which field a scheme reads is its own business — basic prefers
			// username/password and ignores the token. What has to hold for all
			// of them is that a credential goes out and that it tracks the
			// configuration, so a builder cannot quietly drop the secret.
			t.Setenv("TOK", "tok-one")
			t.Setenv("USER", "user-one")
			t.Setenv("PASS", "pass-one")
			first := authHeaderFor(t, authBlock(scheme))
			require.NotEmpty(t, first, "a supported scheme has to send a credential")

			t.Setenv("TOK", "tok-two")
			t.Setenv("USER", "user-two")
			t.Setenv("PASS", "pass-two")
			second := authHeaderFor(t, authBlock(scheme))
			require.NotEqual(t, first, second,
				"the credential has to come from the configuration, not a constant")
		})
	}
}

func TestUndeclaredAuthSchemeIsNotSupported(t *testing.T) {
	for _, scheme := range []string{"digest", "negotiate", "", "bearer-ish"} {
		require.False(t, isSupportedAuthScheme(scheme), "scheme=%q", scheme)
		require.NotContains(t, supportedAuthSchemes(), scheme)
	}
}

// The lookup is on the scheme as the accept file spells it, so casing in the
// file cannot change whether a credential is sent.
func TestAuthSchemeLookupIsCaseInsensitive(t *testing.T) {
	for _, spelling := range []string{"bearer", "Bearer", "BEARER", "BeArEr"} {
		require.True(t, isSupportedAuthScheme(spelling), "spelling=%q", spelling)
	}
}
