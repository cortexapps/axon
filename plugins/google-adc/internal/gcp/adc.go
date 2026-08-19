// Package gcp obtains Google credentials for the agent from Application
// Default Credentials.
//
// It provides two shapes over one mint path. ADCProvider keeps the library's
// in-memory cache and is what runs inside a long-lived process. CachedProvider
// puts a file-backed cache in front of the same mint and is what the
// google-adc plugin binary runs, so a process that exits still leaves a reusable
// token behind.
//
// The plugin is the deployed shape. The token is minted in the customer's
// network and injected on the way upstream, so Cortex holds no Google credential
// and no Google access token travels to Cortex.
//
// A subprocess is used rather than in-process code for one reason: the file
// cache outlives the agent's own restarts. The relay is rebuilt on its idle
// timeout, which discards an in-memory token every few minutes on a link that is
// not busy, and re-runs credential detection each time.
package gcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"github.com/cortexapps/axon/plugins/google-adc/internal/redact"
	"go.uber.org/zap"
)

// ScopeCloudPlatform is the only scope the agent requests.
//
// It is deliberately broader than the calls Cortex makes. The customer's IAM
// bindings are the permission boundary, not this value, so narrowing it buys
// nothing on its own - which is also why it is one constant rather than a
// string threaded through call sites. The pilot should establish whether
// cloud-platform.read-only suffices; if it does, this line is the whole change.
//
// That reasoning only holds while the IAM guidance is genuinely least
// privilege. The least-privilege IAM matrix in the deployment handover package
// is a control this provider depends on, not documentation about it.
const ScopeCloudPlatform = "https://www.googleapis.com/auth/cloud-platform"

// mintTimeout bounds a token exchange.
//
// It is applied twice, because neither placement covers the other. As the HTTP
// client's timeout it reaches the token exchange the library performs on a
// detached context during an asynchronous refresh, which no caller's deadline
// reaches. As a deadline in token() it reaches a cold blocking mint whose caller
// supplied no deadline of its own.
//
// The client timeout does not reach the metadata server: that path is built
// inside the library with its own client (UseDefaultClient), bounded at 5 seconds
// per attempt. The deadline does reach it, because the metadata request is built
// with the caller's context.
//
// 10 seconds does not cover the metadata client's full retry budget. It allows 5
// retries with backoff doubling from 100ms, so a persistently failing metadata
// server can want ~33 seconds and will be cut off here at roughly two attempts.
// That is the intended trade: a request that waits 33 seconds for a credential
// has already failed as far as the caller is concerned.
const mintTimeout = 10 * time.Second

// ErrCredentialsUnavailable means no usable credential configuration was found,
// or the one found could not be read. It is a startup condition: the deployment
// is wrong, and no request will succeed until it is fixed.
var ErrCredentialsUnavailable = errors.New("google credentials are not available")

// ErrTokenMint means a credential exists but the exchange failed. Callers must
// keep this distinct from a Google 401 or 403: those say the identity is not
// permitted to do something, this says the agent never obtained an identity at
// all, and the two lead to opposite fixes.
var ErrTokenMint = errors.New("google token mint failed")

// detectFunc is the seam for tests. Exercising a real detection needs process
// wide environment state, so error mapping is tested through this instead.
type detectFunc func(*credentials.DetectOptions) (*auth.Credentials, error)

// ADCProvider produces an Authorization header value from Application Default
// Credentials. One instance must be shared by every caller that wants token
// reuse, because the cache lives in the credential it holds.
type ADCProvider struct {
	creds       *auth.Credentials
	logger      *zap.Logger
	mintTimeout time.Duration
}

type option func(*providerOptions)

type providerOptions struct {
	detect      detectFunc
	mintTimeout time.Duration
}

// withDetect replaces credential detection. Test-only.
func withDetect(detect detectFunc) option {
	return func(o *providerOptions) {
		o.detect = detect
	}
}

// withMintTimeout shortens the exchange bound. Test-only, so that a test for
// an unresponsive endpoint does not have to wait the real timeout.
func withMintTimeout(timeout time.Duration) option {
	return func(o *providerOptions) {
		o.mintTimeout = timeout
	}
}

// NewADCProvider detects Application Default Credentials once, so a missing or
// malformed configuration fails at startup rather than on the first relayed
// request.
//
// Detection does not mint a token. On a metadata deployment it probes the
// metadata server; with a credential file it parses the file.
func NewADCProvider(logger *zap.Logger, opts ...option) (*ADCProvider, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	logger = logger.Named("gcp-adc")

	options := &providerOptions{
		detect:      credentials.DetectDefault,
		mintTimeout: mintTimeout,
	}
	for _, opt := range opts {
		opt(options)
	}

	// Everything the library caches is left at its default on purpose: the
	// 225-second refresh margin, asynchronous refresh, and the guard that turns
	// concurrent cold calls into one exchange. Setting EarlyTokenRefresh or
	// DisableAsyncRefresh here would replace behavior that was measured with
	// behavior that was guessed.
	creds, err := options.detect(&credentials.DetectOptions{
		Scopes: []string{ScopeCloudPlatform},
		Client: &http.Client{Timeout: options.mintTimeout},
	})
	if err != nil {
		// The library's message can name a credential file and quote part of its
		// contents.
		return nil, fmt.Errorf("%w: %s", ErrCredentialsUnavailable, redact.Redact(err.Error()))
	}

	logger.Info("Detected Google application default credentials",
		zap.Duration("mintTimeout", options.mintTimeout),
		zap.String("scope", ScopeCloudPlatform),
	)

	return &ADCProvider{
		creds:       creds,
		logger:      logger,
		mintTimeout: options.mintTimeout,
	}, nil
}

// Execute returns a complete Authorization header value.
//
// The returned error carries no token, assertion, or credential contents, and
// is never a Google status code.
func (p *ADCProvider) Execute(ctx context.Context) (string, error) {
	value, _, err := p.token(ctx)
	return value, err
}

// token returns the header value and the moment it expires. The token type comes
// from the library rather than being assumed here, so a credential that mints
// something other than a bearer token is described correctly.
//
// The expiry is returned rather than discarded because a cache that outlives this
// process has to store the bound the issuer stated. Deriving one instead would be
// a guess, and a guessed lifetime on a persisted credential fails in the
// direction of serving an expired token.
func (p *ADCProvider) token(ctx context.Context) (string, time.Time, error) {
	// A bound on this call as well as on the client: the client timeout covers
	// the detached refresh, and this covers a cold blocking mint whose caller
	// supplied no deadline of its own.
	ctx, cancel := context.WithTimeout(ctx, p.mintTimeout)
	defer cancel()

	start := time.Now()
	token, err := p.creds.Token(ctx)
	if err != nil {
		// An identity provider reports a failed exchange by echoing part of the
		// request, so this string can contain the assertion or subject token the
		// agent just sent.
		safe := redact.Redact(err.Error())
		p.logger.Error("Failed to mint a Google access token",
			zap.Duration("elapsed", time.Since(start)),
			zap.String("reason", safe),
		)
		return "", time.Time{}, fmt.Errorf("%w: %s", ErrTokenMint, safe)
	}

	tokenType := token.Type
	if tokenType == "" {
		// Documented library default for an uninitialized type.
		tokenType = "Bearer"
	}

	// Type and expiry only. The value is the credential, and Credentials.JSON()
	// is the credential configuration.
	p.logger.Debug("Minted a Google access token",
		zap.String("type", tokenType),
		zap.Time("expiry", token.Expiry),
		zap.Duration("elapsed", time.Since(start)),
	)

	return tokenType + " " + token.Value, token.Expiry, nil
}
