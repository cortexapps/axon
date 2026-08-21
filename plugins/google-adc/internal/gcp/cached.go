package gcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cortexapps/axon/plugins/google-adc/internal/tokencache"
	"go.uber.org/zap"
)

// RefreshMargin restates the auth library's unexported defaultExpiryDelta. The two
// must agree, or the file cache and the library would disagree about which tokens
// are still usable.
const RefreshMargin = 225 * time.Second

const CacheName = "google-adc"

const CacheDirEnv = "AXON_CREDENTIAL_CACHE_DIR"

// CachedProvider is an ADC credential behind a cache that outlives the process.
//
// Credential detection is deferred until a mint is needed, because a cache hit
// must not touch the metadata server.
type CachedProvider struct {
	cache       *tokencache.Cache
	newProvider func() (*ADCProvider, error)

	mu       sync.Mutex
	provider *ADCProvider
}

// NewCachedProvider prepares the cache. It performs no network access and no
// credential detection, so it cannot report a misconfigured deployment - use
// Probe for that.
func NewCachedProvider(logger *zap.Logger, opts ...option) (*CachedProvider, error) {
	if logger == nil {
		logger = zap.NewNop()
	}

	cache, err := tokencache.New(
		CacheDir(),
		CacheName,
		IdentityFingerprint(ScopeCloudPlatform),
		RefreshMargin,
		logger,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrCredentialsUnavailable, err)
	}

	return &CachedProvider{
		cache: cache,
		newProvider: func() (*ADCProvider, error) {
			return NewADCProvider(logger, opts...)
		},
	}, nil
}

// CacheDir reports where the credential is kept. The default is a dedicated
// directory rather than the temporary directory itself, which is world-writable:
// a directory the cache owns can be required to be private.
func CacheDir() string {
	if dir := os.Getenv(CacheDirEnv); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), "axon-credentials")
}

// Execute returns a complete Authorization header value, from the cache when one
// is usable and from a fresh exchange otherwise.
//
// A cold mint blocks the caller rather than being handed to a background refresh:
// a process that exits cannot refresh behind anyone.
func (p *CachedProvider) Execute(ctx context.Context) (string, error) {
	return p.cache.GetOrMint(ctx, func(ctx context.Context) (string, time.Time, error) {
		provider, err := p.detectOnce()
		if err != nil {
			return "", time.Time{}, err
		}
		return provider.token(ctx)
	})
}

func (p *CachedProvider) detectOnce() (*ADCProvider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.provider != nil {
		return p.provider, nil
	}
	provider, err := p.newProvider()
	if err != nil {
		return nil, err
	}
	p.provider = provider
	return provider, nil
}

// Probe reports whether a credential configuration exists and can be read, without
// minting anything. It is what preserves failing at startup, since detection is
// otherwise deferred to the first request.
func Probe(logger *zap.Logger) error {
	_, err := NewADCProvider(logger)
	return err
}

// RunPlugin is the google-adc binary's whole behaviour, kept here so it is
// testable in a package the race detector already covers. Exactly the credential
// is written to out, with no trailing newline and nothing else.
func RunPlugin(ctx context.Context, out io.Writer, logger *zap.Logger, probe bool) error {
	if probe {
		return Probe(logger)
	}

	provider, err := NewCachedProvider(logger)
	if err != nil {
		return err
	}

	value, err := provider.Execute(ctx)
	if err != nil {
		return err
	}
	if value == "" {
		return fmt.Errorf("%w: the provider returned an empty credential", ErrTokenMint)
	}

	_, err = io.WriteString(out, value)
	return err
}
