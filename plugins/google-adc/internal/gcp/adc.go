// Package gcp obtains Google credentials from Application Default Credentials.
//
// ADCProvider keeps the auth library's in-memory cache and suits a long-lived
// process. CachedProvider puts a file-backed cache in front of the same mint, so
// the google-adc plugin binary leaves a reusable token behind when it exits.
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

// ScopeCloudPlatform is the only scope the agent requests. It is deliberately
// broader than the calls Cortex makes: the customer's IAM bindings are the
// permission boundary, not this value.
const ScopeCloudPlatform = "https://www.googleapis.com/auth/cloud-platform"

// mintTimeout bounds a token exchange. It is applied both as the HTTP client's
// timeout, which reaches the detached context the library refreshes on, and as a
// deadline in token(), which reaches a cold mint whose caller supplied none.
const mintTimeout = 10 * time.Second

// ErrCredentialsUnavailable means no credential configuration was found, or the
// one found could not be read.
var ErrCredentialsUnavailable = errors.New("google credentials are not available")

// ErrTokenMint means a credential exists but the exchange failed. Callers must
// keep this distinct from a Google 401 or 403: those say the identity is not
// permitted to do something, this says the agent never obtained an identity.
var ErrTokenMint = errors.New("google token mint failed")

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

func withDetect(detect detectFunc) option {
	return func(o *providerOptions) {
		o.detect = detect
	}
}

func withMintTimeout(timeout time.Duration) option {
	return func(o *providerOptions) {
		o.mintTimeout = timeout
	}
}

// NewADCProvider detects Application Default Credentials once, so a missing or
// malformed configuration fails at startup rather than on the first relayed
// request. Detection does not mint a token.
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

	// The library's caching defaults are left alone on purpose: the 225-second
	// refresh margin, asynchronous refresh, and the guard that collapses
	// concurrent cold calls were measured, and overriding them here would not be.
	creds, err := options.detect(&credentials.DetectOptions{
		Scopes: []string{ScopeCloudPlatform},
		Client: &http.Client{Timeout: options.mintTimeout},
	})
	if err != nil {
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

// Execute fails with an error that carries no credential contents and is never a
// Google status code.
func (p *ADCProvider) Execute(ctx context.Context) (string, error) {
	value, _, err := p.token(ctx)
	return value, err
}

// token returns the header value and the moment it expires. The expiry comes from
// the issuer rather than being derived, because a cache that outlives this process
// has to store the bound the issuer stated.
func (p *ADCProvider) token(ctx context.Context) (string, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, p.mintTimeout)
	defer cancel()

	start := time.Now()
	token, err := p.creds.Token(ctx)
	if err != nil {
		// An identity provider reports a failed exchange by echoing part of the
		// request, so this string can carry the assertion the agent just sent.
		safe := redact.Redact(err.Error())
		p.logger.Error("Failed to mint a Google access token",
			zap.Duration("elapsed", time.Since(start)),
			zap.String("reason", safe),
		)
		return "", time.Time{}, fmt.Errorf("%w: %s", ErrTokenMint, safe)
	}

	tokenType := token.Type
	if tokenType == "" {
		tokenType = "Bearer"
	}

	p.logger.Debug("Minted a Google access token",
		zap.String("type", tokenType),
		zap.Time("expiry", token.Expiry),
		zap.Duration("elapsed", time.Since(start)),
	)

	return tokenType + " " + token.Value, token.Expiry, nil
}
