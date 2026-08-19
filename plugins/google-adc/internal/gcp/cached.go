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

// RefreshMargin mirrors the auth library's own default (defaultExpiryDelta, 225
// seconds). It is restated rather than imported because the library does not
// export it.
//
// The two must agree. If the file cache served a token the library considers
// stale, the in-process path and the subprocess path would disagree about which
// tokens are usable, and the symptom would be an expiry that depends on which one
// answered.
const RefreshMargin = 225 * time.Second

// CacheName is the cache file's stem. It matches the reserved accept-file source
// name, so an operator looking at the directory can tell what wrote it.
const CacheName = "google-adc"

// CacheDirEnv overrides where the token is kept. It exists for tests and for a
// deployment that mounts something other than the default temporary directory.
const CacheDirEnv = "AXON_CREDENTIAL_CACHE_DIR"

// CachedProvider is an ADC credential behind a cache that outlives the process.
//
// Detection is deferred. A cache hit must not touch the metadata server, which is
// the whole reason the file exists, so the credential is only detected when a
// mint is actually needed.
type CachedProvider struct {
	cache *tokencache.Cache

	// detect is the seam for tests and the memoised construction of the
	// underlying provider.
	detect func() (*ADCProvider, error)

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
	// Not named here: NewADCProvider names the same logger, and naming it twice
	// produces "gcp-adc.gcp-adc" in every line.

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
		detect: func() (*ADCProvider, error) {
			return NewADCProvider(logger, opts...)
		},
	}, nil
}

// CacheDir reports where the credential is kept.
//
// The default is a dedicated directory rather than the temporary directory
// itself, which is world-writable: a directory the cache owns can be required to
// be private, while a file sitting directly in /tmp can be pre-created by anything
// else on the host.
func CacheDir() string {
	if dir := os.Getenv(CacheDirEnv); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), "axon-credentials")
}

// Execute returns a complete Authorization header value, from the cache when one
// is usable and from a fresh exchange otherwise.
//
// A cold mint blocks the caller rather than being handed off to a background
// refresh. A process that exits cannot refresh behind anyone, and a detached
// child would be an unsupervised process holding a credential. The cost is one
// slow request per token lifetime.
func (p *CachedProvider) Execute(ctx context.Context) (string, error) {
	return p.cache.GetOrMint(ctx, func(ctx context.Context) (string, time.Time, error) {
		provider, err := p.provide()
		if err != nil {
			return "", time.Time{}, err
		}
		return provider.token(ctx)
	})
}

// provide detects once per process. Within the plugin that is once per
// invocation; in a long-lived process it keeps the library's in-memory cache
// alive alongside the file, so a warm process answers without either.
func (p *CachedProvider) provide() (*ADCProvider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.provider != nil {
		return p.provider, nil
	}
	provider, err := p.detect()
	if err != nil {
		return nil, err
	}
	p.provider = provider
	return provider, nil
}

// Probe reports whether a credential configuration exists and can be read,
// without minting anything.
//
// This is what preserves failing at startup. Detection is otherwise deferred to
// the first request, and a deployment with no credential would look healthy until
// traffic arrived. It deliberately does not mint: a transient network fault
// during an exchange is not a reason to refuse to start.
func Probe(logger *zap.Logger) error {
	_, err := NewADCProvider(logger)
	return err
}

// RunPlugin is the google-adc binary's whole behaviour, kept here so it is
// testable in a package the race detector already covers.
//
// Exactly the credential is written to out, with no trailing newline and nothing
// else: stdout is the header value. Diagnostics belong on the logger, which the
// caller points at stderr.
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
		// Unreachable through Execute, and worth stopping anyway: an empty
		// credential written to stdout becomes an empty Authorization header,
		// which the upstream answers with a 401 that looks like a permission
		// problem.
		return fmt.Errorf("%w: the provider returned an empty credential", ErrTokenMint)
	}

	_, err = io.WriteString(out, value)
	return err
}
