package acceptfile

import (
	"encoding/base64"
	"net/http"
	"os"
	"sort"
	"strings"
)

// authScheme is an accept-file auth scheme, always lowercased. Go has no enum,
// but a named type keeps the scheme from being just another string in flight
// and lets the declaration below and its uses refer to the same symbols.
type authScheme string

const (
	authSchemeBearer authScheme = "bearer"
	authSchemeToken  authScheme = "token"
	authSchemeBasic  authScheme = "basic"
	authSchemeRaw    authScheme = "raw"
)

// authHeaderBuilders is the single declaration of which schemes Axon supports:
// a scheme is supported exactly when there is a builder for it here.
//
// The list and the behaviour have to be one table, not two. They were a set in
// supported.go and a switch in router.go, which could drift in either
// direction — a scheme in the set with no case sends no credential while
// claiming to be supported, and a case with no set entry warns about itself
// while working fine.
//
// The bodies match snyk-broker's lib/common/utils/auth-header.ts.
var authHeaderBuilders = map[authScheme]func(auth *AcceptFileRuleAuth) string{
	authSchemeBearer: func(auth *AcceptFileRuleAuth) string {
		return "Bearer " + os.ExpandEnv(auth.Token)
	},
	authSchemeToken: func(auth *AcceptFileRuleAuth) string {
		return "Token " + os.ExpandEnv(auth.Token)
	},
	// The upstream's header carries no scheme prefix at all.
	authSchemeRaw: func(auth *AcceptFileRuleAuth) string {
		return os.ExpandEnv(auth.Token)
	},
	authSchemeBasic: func(auth *AcceptFileRuleAuth) string {
		return "Basic " + basicCredential(auth)
	},
}

// authHeaderBuilder returns how to build the header for a scheme as written in
// the accept file, and whether the scheme is one Axon supports at all.
func authHeaderBuilder(scheme string) (func(*AcceptFileRuleAuth) string, bool) {
	build, ok := authHeaderBuilders[authScheme(strings.ToLower(scheme))]
	return build, ok
}

// isSupportedAuthScheme reports whether the Router can build a credential for
// the scheme as the accept file spells it.
func isSupportedAuthScheme(scheme string) bool {
	_, ok := authHeaderBuilder(scheme)
	return ok
}

// supportedAuthSchemes lists the schemes in a stable order, for a message that
// has to name them.
func supportedAuthSchemes() []string {
	names := make([]string, 0, len(authHeaderBuilders))
	for scheme := range authHeaderBuilders {
		names = append(names, string(scheme))
	}
	sort.Strings(names)
	return names
}

// applyAuth sets the Authorization header from the rule's auth block.
//
// An unrecognized scheme sets no header at all, which is what snyk-broker's
// authHeader() does with one; warnUnsupportedRule has already said so when the
// Router was built.
func applyAuth(header http.Header, auth *AcceptFileRuleAuth) {
	if auth == nil {
		return
	}
	if build, ok := authHeaderBuilder(auth.Scheme); ok {
		header.Set("Authorization", build(auth))
	}
}

// basicCredential builds the base64 payload. A basic block carrying "token"
// holds the already-encoded user:pass pair (the shape Azure Repos uses), so
// encoding it again would send a broken credential.
func basicCredential(auth *AcceptFileRuleAuth) string {
	if auth.Username != "" || auth.Password != "" {
		username := os.ExpandEnv(auth.Username)
		password := os.ExpandEnv(auth.Password)
		return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	}
	return os.ExpandEnv(auth.Token)
}
