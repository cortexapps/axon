package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// newCapturingLogger returns a logger and a reader over everything it emitted,
// for the tests that assert a value never appeared in a log line.
func newCapturingLogger() (*zap.Logger, func() string) {
	buf := &syncBuffer{}
	logger := zap.New(zapcore.NewCore(
		zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig()),
		zapcore.AddSync(buf),
		zapcore.DebugLevel,
	))
	return logger, buf.contents
}

type syncBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *syncBuffer) contents() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

// The metadata deployment is the ordinary GKE Workload Identity case: no
// credential file exists, and the token comes from the metadata server.
func TestMetadataPathMintsAToken(t *testing.T) {
	var mu sync.Mutex
	var flavor, scopes string

	fakeMetadataServer.install(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		flavor = r.Header.Get("Metadata-Flavor")
		scopes = r.URL.Query().Get("scopes")
		mu.Unlock()
		respondWithToken("metadata-token-value", 3600)(w, r)
	})

	provider, err := NewADCProvider(zap.NewNop())
	require.NoError(t, err)

	value, err := provider.Execute(context.Background())
	require.NoError(t, err)
	require.Equal(t, "Bearer metadata-token-value", value)

	mu.Lock()
	defer mu.Unlock()
	// Without this header the real metadata server refuses the request, so a
	// fake that does not check it would pass while production failed.
	require.Equal(t, "Google", flavor)
	require.Equal(t, ScopeCloudPlatform, scopes)
}

// The whole reason this provider runs in process. If the token were not reused,
// every relayed request would mint one.
func TestTokenIsReusedAcrossCalls(t *testing.T) {
	fakeMetadataServer.install(t, respondWithToken("reused-token", 3600))

	provider, err := NewADCProvider(zap.NewNop())
	require.NoError(t, err)

	for i := 0; i < 20; i++ {
		value, err := provider.Execute(context.Background())
		require.NoError(t, err)
		require.Equal(t, "Bearer reused-token", value)
	}

	require.Equal(t, 1, fakeMetadataServer.mintCount())
}

// A cold start is the one moment every caller wants a token at once. The library
// serializes them, so nothing here has to.
func TestConcurrentColdCallsMintOnce(t *testing.T) {
	fakeMetadataServer.install(t, respondWithToken("single-mint-token", 3600))

	provider, err := NewADCProvider(zap.NewNop())
	require.NoError(t, err)

	const callers = 64
	var wg sync.WaitGroup
	values := make([]string, callers)
	errs := make([]error, callers)

	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			values[i], errs[i] = provider.Execute(context.Background())
		}(i)
	}
	wg.Wait()

	for i := 0; i < callers; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, "Bearer single-mint-token", values[i])
	}
	require.Equal(t, 1, fakeMetadataServer.mintCount())
}

// Pins two library behaviours the provider relies on and deliberately does not
// configure: the refresh margin is 225 seconds, and the refresh is
// asynchronous. A token valid for 200 seconds is inside the margin, so it is
// due for refresh while still usable - and the caller that triggers the refresh
// gets the old value straight away rather than waiting for the new one.
//
// If a future library version shortened the margin to, say, 10 seconds, the
// second call here would return the second token and this test would fail. That
// is the point: the margin is what keeps a refresh from colliding with the
// 30-second read timeout above this path.
func TestRefreshMarginIsAsynchronousAndWideEnough(t *testing.T) {
	var mu sync.Mutex
	minted := 0

	// The first token is inside the refresh margin, so it is stale on arrival.
	// The replacement is not, so the provider quiesces once the refresh lands -
	// which both keeps this test's own background work bounded and lets the
	// final count prove no refresh was still in flight when it returned.
	fakeMetadataServer.install(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		minted++
		first := minted == 1
		mu.Unlock()

		if first {
			respondWithToken("token-a", 200)(w, r)
			return
		}
		respondWithToken("token-b", 3600)(w, r)
	})

	provider, err := NewADCProvider(zap.NewNop())
	require.NoError(t, err)

	first, err := provider.Execute(context.Background())
	require.NoError(t, err)
	require.Equal(t, "Bearer token-a", first)

	// Still token-a: 200 seconds is inside the 225-second margin, so this call
	// finds the token due for refresh - and gets the old value straight away
	// rather than waiting for the new one.
	second, err := provider.Execute(context.Background())
	require.NoError(t, err)
	require.Equal(t, "Bearer token-a", second)

	require.Eventually(t, func() bool {
		value, err := provider.Execute(context.Background())
		return err == nil && value == "Bearer token-b"
	}, 5*time.Second, 10*time.Millisecond, "the background refresh never replaced the token")

	// Exactly two: the fresh replacement stopped the refreshing, so nothing is
	// left running to disturb a later test.
	require.Equal(t, 2, fakeMetadataServer.mintCount())
}

// Workload identity federation: the customer runs outside GCE, and a credential
// configuration file names where the exchange happens and where the subject
// token comes from.
func TestExternalFederationExchangesAtTheConfiguredTokenURL(t *testing.T) {
	var mu sync.Mutex
	var exchanges int
	var sawSubjectToken bool

	sts := newSTSServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		mu.Lock()
		exchanges++
		sawSubjectToken = strings.Contains(string(body), "subject_token=")
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":      "federated-token-value",
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
			"token_type":        "Bearer",
			"expires_in":        3600,
		})
	})

	fakeMetadataServer.refuseEverything(t)
	useCredentialConfig(t, writeCredentialConfig(t, sts))

	provider, err := NewADCProvider(zap.NewNop())
	require.NoError(t, err)

	value, err := provider.Execute(context.Background())
	require.NoError(t, err)
	require.Equal(t, "Bearer federated-token-value", value)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, exchanges)
	require.True(t, sawSubjectToken, "the subject token from credential_source was not exchanged")

	// The metadata server must not have been consulted: a credential file wins,
	// and a deployment that silently fell back to the node identity would be
	// using the wrong identity entirely.
	require.Equal(t, 0, fakeMetadataServer.mintCount())
}

// Workload identity federation with service account impersonation: the shape a
// customer under a no-static-credentials policy hands over, because the
// configuration file carries no secret. It names the pool, where to read this
// workload's own token, and which service account to impersonate.
//
// The chain has two legs. The subject token is exchanged at STS for a federated
// token, and that federated token authorizes a generateAccessToken call on the
// target service account. What goes upstream is the second token, not the first.
func TestFederationWithImpersonationReturnsTheImpersonatedToken(t *testing.T) {
	var mu sync.Mutex
	var stsExchanges, impersonations int
	var impersonationAuth string
	var impersonationBody string

	sts := newSTSServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stsExchanges++
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":      "federated-token-value",
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
			"token_type":        "Bearer",
			"expires_in":        3600,
		})
	})

	impersonation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		mu.Lock()
		impersonations++
		impersonationAuth = r.Header.Get("Authorization")
		impersonationBody = string(body)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "impersonated-token-value",
			"expireTime":  time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(impersonation.Close)

	fakeMetadataServer.refuseEverything(t)
	useCredentialConfig(t, writeImpersonationConfig(t, sts,
		impersonation.URL+"/v1/projects/-/serviceAccounts/axon@customer.iam.gserviceaccount.com:generateAccessToken"))

	provider, err := NewADCProvider(zap.NewNop())
	require.NoError(t, err)

	value, err := provider.Execute(context.Background())
	require.NoError(t, err)

	// The impersonated token, not the federated one. Sending the federated token
	// upstream would be acting as the pool identity rather than as the service
	// account the customer granted access to.
	require.Equal(t, "Bearer impersonated-token-value", value)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, stsExchanges)
	require.Equal(t, 1, impersonations)
	require.Equal(t, "Bearer federated-token-value", impersonationAuth,
		"the impersonation call was not authorized with the federated token")

	// Under impersonation the library pins the STS exchange to cloud-platform and
	// applies the requested scope to this call instead. So ScopeCloudPlatform is
	// what bounds the token that actually goes upstream, and narrowing it later
	// takes effect here.
	require.Contains(t, impersonationBody, ScopeCloudPlatform,
		"the requested scope did not reach the impersonation call")

	require.Equal(t, 0, fakeMetadataServer.mintCount())
}

// The bound has to be on the client, not on the caller's context, because the
// library refreshes on a detached context. This asserts both that the real value
// is 10 seconds and that the bound actually fires.
func TestMintTimeoutBoundsAnUnresponsiveEndpoint(t *testing.T) {
	require.Equal(t, 10*time.Second, mintTimeout,
		"the mint timeout must stay below the read timeout above this path and above the metadata retry window")

	useCredentialConfig(t, writeCredentialConfig(t, newSilentEndpoint(t)))

	provider, err := NewADCProvider(zap.NewNop(), withMintTimeout(500*time.Millisecond))
	require.NoError(t, err)

	start := time.Now()
	_, err = provider.Execute(context.Background())
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrTokenMint)
	require.Less(t, elapsed, 5*time.Second, "the exchange was not bounded by the configured timeout")
}

// An identity provider reports a failed exchange by echoing the request back.
// Neither the returned error nor the log may carry what it echoed.
func TestProviderErrorsAndLogsAreRedacted(t *testing.T) {
	const subjectToken = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJheG9uIn0.ZmFrZS1zaWduYXR1cmU"
	const leakedAccessToken = "ya29.leaked-access-token-value"

	sts := newSTSServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":             "invalid_request",
			"error_description": "audience mismatch",
			"subject_token":     subjectToken,
			"access_token":      leakedAccessToken,
		})
	})

	useCredentialConfig(t, writeCredentialConfig(t, sts))

	logger, logged := newCapturingLogger()
	provider, err := NewADCProvider(logger)
	require.NoError(t, err)

	_, err = provider.Execute(context.Background())
	require.ErrorIs(t, err, ErrTokenMint)

	require.NotContains(t, err.Error(), subjectToken)
	require.NotContains(t, err.Error(), leakedAccessToken)
	require.NotContains(t, logged(), subjectToken)
	require.NotContains(t, logged(), leakedAccessToken)

	// Redaction that removed the diagnosis too would just get deleted later.
	require.Contains(t, err.Error(), "invalid_request")
}

// A missing or unreadable configuration is a deployment fault, and it must be
// reported at construction so the agent fails at startup rather than on the
// first relayed request.
func TestUnavailableCredentialsAreTypedSeparately(t *testing.T) {
	detectFailed := func(*credentials.DetectOptions) (*auth.Credentials, error) {
		return nil, errors.New(`credentials: could not find default credentials, read "ya29.secret-in-a-message"`)
	}

	_, err := NewADCProvider(zap.NewNop(), withDetect(detectFailed))

	require.ErrorIs(t, err, ErrCredentialsUnavailable)
	require.NotErrorIs(t, err, ErrTokenMint)
	require.NotContains(t, err.Error(), "ya29.secret-in-a-message")
}

// The two failure kinds must stay distinguishable, because a mint failure and a
// Google authorization failure lead to opposite fixes: one is the agent's
// deployment, the other is the customer's IAM.
func TestMintFailureIsNotACredentialConfigurationFailure(t *testing.T) {
	sts := newSTSServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	})

	useCredentialConfig(t, writeCredentialConfig(t, sts))

	provider, err := NewADCProvider(zap.NewNop())
	require.NoError(t, err, "detection succeeds; only the exchange fails")

	_, err = provider.Execute(context.Background())
	require.ErrorIs(t, err, ErrTokenMint)
	require.NotErrorIs(t, err, ErrCredentialsUnavailable)
}

// The scope is one constant so that narrowing it after the pilot is a one-line
// change rather than a search across call sites.
func TestScopeIsCloudPlatform(t *testing.T) {
	require.Equal(t, "https://www.googleapis.com/auth/cloud-platform", ScopeCloudPlatform)
}
