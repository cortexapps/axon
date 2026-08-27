package acceptfile

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// Pool rotation has to happen where the origin is resolved for a request.
// Resolving ${VAR} against the environment first turns a pool-only variable
// into the empty string, and the PoolManager then has nothing left to rotate —
// so every request fails while pool_test.go stays green.

func TestRouterRotatesPoolOrigins(t *testing.T) {
	t.Setenv("POOLED_API_POOL", "https://a.example.com,https://b.example.com,https://c.example.com")

	rt := newTestRouter(t, `{"private":[{"method":"any","path":"/*","origin":"${POOLED_API}"}]}`)

	want := []string{
		"https://a.example.com/x", "https://b.example.com/x", "https://c.example.com/x",
		"https://a.example.com/x", "https://b.example.com/x", "https://c.example.com/x",
	}
	for i, w := range want {
		routed, err := rt.Route("GET", "/x", nil)
		require.NoError(t, err, "request %d", i)
		require.Equal(t, w, routed.URL.String(), "request %d", i)
	}
}

func TestRouterPoolTrimsWhitespaceAndEmptyEntries(t *testing.T) {
	t.Setenv("POOLED_API_POOL", " https://a.example.com , ,https://b.example.com ")

	rt := newTestRouter(t, `{"private":[{"method":"any","path":"/*","origin":"${POOLED_API}"}]}`)

	for _, w := range []string{"https://a.example.com/x", "https://b.example.com/x", "https://a.example.com/x"} {
		routed, err := rt.Route("GET", "/x", nil)
		require.NoError(t, err)
		require.Equal(t, w, routed.URL.String())
	}
}

// A pool of one is a plain origin.
func TestRouterPoolOfOne(t *testing.T) {
	t.Setenv("POOLED_API_POOL", "https://only.example.com")

	rt := newTestRouter(t, `{"private":[{"method":"any","path":"/*","origin":"${POOLED_API}"}]}`)
	for i := 0; i < 3; i++ {
		routed, err := rt.Route("GET", "/x", nil)
		require.NoError(t, err)
		require.Equal(t, "https://only.example.com/x", routed.URL.String())
	}
}

// With no pool set the plain environment variable still wins.
func TestRouterFallsBackToPlainEnvVar(t *testing.T) {
	t.Setenv("SINGLE_API", "https://api.example.com")

	rt := newTestRouter(t, `{"private":[{"method":"any","path":"/*","origin":"${SINGLE_API}"}]}`)
	routed, err := rt.Route("GET", "/x", nil)
	require.NoError(t, err)
	require.Equal(t, "https://api.example.com/x", routed.URL.String())
}

// A pool beats the plain variable when both are set, matching the broker.
func TestRouterPoolBeatsPlainEnvVar(t *testing.T) {
	t.Setenv("BOTH_API", "https://plain.example.com")
	t.Setenv("BOTH_API_POOL", "https://pooled.example.com")

	rt := newTestRouter(t, `{"private":[{"method":"any","path":"/*","origin":"${BOTH_API}"}]}`)
	routed, err := rt.Route("GET", "/x", nil)
	require.NoError(t, err)
	require.Equal(t, "https://pooled.example.com/x", routed.URL.String())
}

// Every concurrent request must land on a real pool member; the counter is the
// only shared state on the hot path.
func TestRouterPoolIsSafeUnderConcurrency(t *testing.T) {
	t.Setenv("POOLED_API_POOL", "https://a.example.com,https://b.example.com")

	rt := newTestRouter(t, `{"private":[{"method":"any","path":"/*","origin":"${POOLED_API}"}]}`)

	const n = 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[string]int{}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			routed, err := rt.Route("GET", "/x", nil)
			if err != nil {
				return
			}
			mu.Lock()
			counts[routed.URL.Host]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	require.Equal(t, n, counts["a.example.com"]+counts["b.example.com"],
		"every request must resolve to a pool member")
	require.Equal(t, n/2, counts["a.example.com"], "rotation must stay even")
	require.Equal(t, n/2, counts["b.example.com"], "rotation must stay even")
}

// A pool-only variable satisfies the accept file's required-variable check
// (varIsSet accepts VAR_POOL), so it must also route.
func TestPoolOnlyVariableSatisfiesLoadValidation(t *testing.T) {
	t.Setenv("DECLARED_API_POOL", "https://a.example.com")

	rt := newTestRouter(t, `{"$vars":["${DECLARED_API}"],
		"private":[{"method":"any","path":"/*","origin":"${DECLARED_API}"}]}`)

	routed, err := rt.Route("GET", "/x", nil)
	require.NoError(t, err)
	require.Equal(t, "https://a.example.com/x", routed.URL.String())
}
