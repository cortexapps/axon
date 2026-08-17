package snykbroker

import (
	"fmt"
	"net/url"
	"sync"
	"testing"

	"github.com/cortexapps/axon/server/snykbroker/acceptfile"
	"github.com/stretchr/testify/require"
)

func proxyPath(t *testing.T, proxyURI string) string {
	u, err := url.Parse(proxyURI)
	require.NoError(t, err)
	return u.Path
}

func TestProxyEntryKeyStableAcrossHeaderOrder(t *testing.T) {
	headers := map[string]string{
		"Authorization":        "Bearer ${plugin:gcp-token}",
		"X-GitHub-Api-Version": "2022-11-28",
		"X-Third":              "three",
	}
	first, err := newProxyEntry("https://example.com", false, 8080, acceptfile.NewResolverMapFromMap(headers), nil)
	require.NoError(t, err)
	// map iteration order is randomized per map instance, so repeated
	// construction flushes out order-dependent hashing
	for i := 0; i < 20; i++ {
		next, err := newProxyEntry("https://example.com", false, 8080, acceptfile.NewResolverMapFromMap(headers), nil)
		require.NoError(t, err)
		require.Equal(t, first.key(), next.key())
	}
}

// TestConcurrentGetProxyAndParseTargetUri reproduces the production race
// between registration writing rr.targets (the broker start path retries
// registration on its own goroutine, so it can register entries long after
// Start returned) and ServeHTTP reading it through parseTargetUri. Mutating
// the map in place makes this a concurrent map read-and-write, which is a Go
// runtime throw rather than a tolerable race. Only meaningful under -race.
func TestConcurrentGetProxyAndParseTargetUri(t *testing.T) {
	env := newTestReflectorEnv(t)

	// seed entries so readers resolve real hashes while writers add more
	seedPaths := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		entry, err := env.Reflector.getProxy(fmt.Sprintf("http://seed-%d.example.com", i), false, nil)
		require.NoError(t, err)
		seedPaths = append(seedPaths, proxyPath(t, entry.proxyURI))
	}

	const workers = 16
	const perWorker = 50
	var wg sync.WaitGroup
	errs := make(chan error, 2*workers*perWorker)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				// a distinct URI per iteration, so every call writes a new entry
				_, err := env.Reflector.getProxy(fmt.Sprintf("http://w%d-i%d.example.com", w, i), false, nil)
				if err != nil {
					errs <- fmt.Errorf("getProxy w=%d i=%d: %w", w, i, err)
					return
				}
			}
		}(w)
	}

	for r := 0; r < workers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				path := seedPaths[(r+i)%len(seedPaths)]
				entry, _, err := env.Reflector.parseTargetUri(path)
				if err != nil {
					errs <- fmt.Errorf("parseTargetUri r=%d i=%d path=%s: %w", r, i, path, err)
					return
				}
				if entry == nil {
					errs <- fmt.Errorf("parseTargetUri r=%d i=%d path=%s: nil entry", r, i, path)
					return
				}
			}
		}(r)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	// every writer URI registered exactly one entry, plus the seeds. A lost
	// copy-on-write update would show up here as a short map.
	require.Len(t, *env.Reflector.targets.Load(), 4+workers*perWorker)
}
