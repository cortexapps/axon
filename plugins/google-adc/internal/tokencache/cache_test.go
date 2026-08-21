package tokencache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const (
	testFingerprint = "abc123"
	testMargin      = 225 * time.Second
	oneHour         = time.Hour
)

// Switch the test binary into the helper mode the cross-process tests use. A
// subprocess is the only way to exercise flock: two goroutines share a file
// descriptor table, so an in-process test would pass with no locking at all.
const (
	childEnv     = "TOKENCACHE_TEST_CHILD_DIR"
	childCounter = "TOKENCACHE_TEST_CHILD_COUNTER"
	childDelay   = "TOKENCACHE_TEST_CHILD_DELAY"
)

func TestMain(m *testing.M) {
	if dir := os.Getenv(childEnv); dir != "" {
		os.Exit(runChild(dir))
	}
	os.Exit(m.Run())
}

// runChild performs one GetOrMint and prints the value, recording the mint by
// appending one byte to a shared file. A single O_APPEND write of one byte is
// atomic, so the file length counts mints across processes exactly.
func runChild(dir string) int {
	counter := os.Getenv(childCounter)
	delay, _ := time.ParseDuration(os.Getenv(childDelay))

	cache, err := New(dir, "child", testFingerprint, testMargin, zap.NewNop())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	value, err := cache.GetOrMint(context.Background(), func(context.Context) (string, time.Time, error) {
		file, err := os.OpenFile(counter, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return "", time.Time{}, err
		}
		defer file.Close()
		if _, err := file.Write([]byte{'x'}); err != nil {
			return "", time.Time{}, err
		}
		// Long enough that the other processes are waiting on the lock rather than
		// finishing before they contend.
		time.Sleep(delay)
		return "Bearer minted-by-a-child", time.Now().Add(oneHour), nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Print(value)
	return 0
}

func newTestCache(t *testing.T, dir string) *Cache {
	t.Helper()
	cache, err := New(dir, "test", testFingerprint, testMargin, zap.NewNop())
	require.NoError(t, err)
	return cache
}

// mintOnce returns a MintFunc and the count of how often it ran.
func mintOnce(value string, ttl time.Duration) (MintFunc, *atomic.Int32) {
	calls := &atomic.Int32{}
	return func(context.Context) (string, time.Time, error) {
		calls.Add(1)
		var expiry time.Time
		if ttl != 0 {
			expiry = time.Now().Add(ttl)
		}
		return value, expiry, nil
	}, calls
}

func TestSecondCallIsServedFromCache(t *testing.T) {
	cache := newTestCache(t, t.TempDir())
	mint, calls := mintOnce("Bearer one", oneHour)

	for i := 0; i < 20; i++ {
		value, err := cache.GetOrMint(context.Background(), mint)
		require.NoError(t, err)
		require.Equal(t, "Bearer one", value)
	}
	require.Equal(t, int32(1), calls.Load())
}

// The property that motivates the package. A fresh Cache over the same directory
// stands in for the next subprocess.
func TestValueSurvivesTheProcessThatMintedIt(t *testing.T) {
	dir := t.TempDir()
	mint, calls := mintOnce("Bearer survivor", oneHour)

	first, err := newTestCache(t, dir).GetOrMint(context.Background(), mint)
	require.NoError(t, err)

	second, err := newTestCache(t, dir).GetOrMint(context.Background(), mint)
	require.NoError(t, err)

	require.Equal(t, first, second)
	require.Equal(t, int32(1), calls.Load(), "the second cache minted again instead of reading the file")
}

// A token inside the margin is still valid, and is deliberately not served: the
// margin exists so the caller never receives one that expires mid-request.
func TestValueInsideTheRefreshMarginIsNotServed(t *testing.T) {
	cache := newTestCache(t, t.TempDir())

	_, err := cache.GetOrMint(context.Background(), func(context.Context) (string, time.Time, error) {
		return "Bearer nearly-expired", time.Now().Add(200 * time.Second), nil
	})
	require.NoError(t, err)

	mint, calls := mintOnce("Bearer replacement", oneHour)
	value, err := cache.GetOrMint(context.Background(), mint)
	require.NoError(t, err)

	require.Equal(t, "Bearer replacement", value)
	require.Equal(t, int32(1), calls.Load())
}

func TestValueOutsideTheRefreshMarginIsServed(t *testing.T) {
	cache := newTestCache(t, t.TempDir())

	_, err := cache.GetOrMint(context.Background(), func(context.Context) (string, time.Time, error) {
		return "Bearer still-good", time.Now().Add(testMargin + time.Minute), nil
	})
	require.NoError(t, err)

	_, calls := mintOnce("Bearer unused", oneHour)
	value, err := cache.GetOrMint(context.Background(), func(ctx context.Context) (string, time.Time, error) {
		calls.Add(1)
		return "Bearer unused", time.Now().Add(oneHour), nil
	})
	require.NoError(t, err)

	require.Equal(t, "Bearer still-good", value)
	require.Equal(t, int32(0), calls.Load())
}

// Without the fingerprint check, changing GOOGLE_APPLICATION_CREDENTIALS would
// keep using the previous identity until its token expired, and present as an IAM
// error on a service account nobody thought was in play.
func TestADifferentIdentityDoesNotReadTheSameCache(t *testing.T) {
	dir := t.TempDir()

	first, err := New(dir, "test", "fingerprintone", testMargin, zap.NewNop())
	require.NoError(t, err)
	_, err = first.GetOrMint(context.Background(), func(context.Context) (string, time.Time, error) {
		return "Bearer identity-one", time.Now().Add(oneHour), nil
	})
	require.NoError(t, err)

	second, err := New(dir, "test", "fingerprinttwo", testMargin, zap.NewNop())
	require.NoError(t, err)
	mint, calls := mintOnce("Bearer identity-two", oneHour)
	value, err := second.GetOrMint(context.Background(), mint)
	require.NoError(t, err)

	require.Equal(t, "Bearer identity-two", value)
	require.Equal(t, int32(1), calls.Load())
	require.NotEqual(t, first.Path(), second.Path(),
		"two identities sharing one path would rewrite each other's cache on every request")
}

// Persisting a credential with no stated expiry would mean choosing a lifetime the
// issuer declined to state, which is worse than minting per process.
func TestAValueWithNoExpiryIsNotCached(t *testing.T) {
	cache := newTestCache(t, t.TempDir())
	mint, calls := mintOnce("Bearer no-expiry", 0)

	for i := 0; i < 3; i++ {
		value, err := cache.GetOrMint(context.Background(), mint)
		require.NoError(t, err)
		require.Equal(t, "Bearer no-expiry", value)
	}

	require.Equal(t, int32(3), calls.Load())
	require.NoFileExists(t, cache.Path())
}

// read would reject it, so writing it would leave a credential on disk that
// nothing can use.
func TestAValueInsideTheMarginIsNotWritten(t *testing.T) {
	cache := newTestCache(t, t.TempDir())

	_, err := cache.GetOrMint(context.Background(), func(context.Context) (string, time.Time, error) {
		return "Bearer already-stale", time.Now().Add(200 * time.Second), nil
	})
	require.NoError(t, err)

	require.NoFileExists(t, cache.Path())
}

func TestCorruptCacheIsReplacedRatherThanReported(t *testing.T) {
	cache := newTestCache(t, t.TempDir())
	require.NoError(t, os.WriteFile(cache.Path(), []byte("{not json"), 0600))

	mint, calls := mintOnce("Bearer after-corruption", oneHour)
	value, err := cache.GetOrMint(context.Background(), mint)
	require.NoError(t, err)

	require.Equal(t, "Bearer after-corruption", value)
	require.Equal(t, int32(1), calls.Load())

	// And the replacement is readable, so one bad file does not poison every later
	// request.
	again, err := cache.GetOrMint(context.Background(), mint)
	require.NoError(t, err)
	require.Equal(t, "Bearer after-corruption", again)
	require.Equal(t, int32(1), calls.Load())
}

// The file holds a live credential, so its mode is part of the contract.
func TestCacheFileIsOwnerOnlyAndLeavesNoTemporaries(t *testing.T) {
	dir := t.TempDir()
	cache := newTestCache(t, dir)

	mint, _ := mintOnce("Bearer private", oneHour)
	_, err := cache.GetOrMint(context.Background(), mint)
	require.NoError(t, err)

	info, err := os.Stat(cache.Path())
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, strings.HasSuffix(e.Name(), ".tmp"),
			"a temporary file was left behind: %s", e.Name())
	}
}

// Necessary but not sufficient - TestConcurrentProcessesMintOnce is the one that
// exercises the lock.
func TestConcurrentCallersInOneProcessMintOnce(t *testing.T) {
	cache := newTestCache(t, t.TempDir())

	const callers = 64
	var wg sync.WaitGroup
	values := make([]string, callers)
	errs := make([]error, callers)
	calls := &atomic.Int32{}

	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			values[i], errs[i] = cache.GetOrMint(context.Background(), func(context.Context) (string, time.Time, error) {
				calls.Add(1)
				time.Sleep(50 * time.Millisecond)
				return "Bearer single", time.Now().Add(oneHour), nil
			})
		}(i)
	}
	wg.Wait()

	for i := 0; i < callers; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, "Bearer single", values[i])
	}
	require.Equal(t, int32(1), calls.Load())
}

// Eight processes start at once on an empty cache; one mints and the rest read
// what it wrote. Without the lock, or with a truncate-in-place write, this fails
// with either eight mints or a caller that read an empty file.
func TestConcurrentProcessesMintOnce(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "mints")

	const children = 8
	cmds := make([]*exec.Cmd, children)
	outputs := make([]*strings.Builder, children)

	for i := 0; i < children; i++ {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(),
			childEnv+"="+dir,
			childCounter+"="+counter,
			childDelay+"=200ms",
		)
		out := &strings.Builder{}
		cmd.Stdout = out
		cmd.Stderr = os.Stderr
		cmds[i] = cmd
		outputs[i] = out
	}

	// Started in one pass so they contend, rather than each finishing before the
	// next begins.
	for _, cmd := range cmds {
		require.NoError(t, cmd.Start())
	}
	for i, cmd := range cmds {
		require.NoError(t, cmd.Wait(), "child %d failed", i)
	}

	for i, out := range outputs {
		require.Equal(t, "Bearer minted-by-a-child", out.String(), "child %d returned the wrong value", i)
	}

	minted, err := os.ReadFile(counter)
	require.NoError(t, err)
	require.Len(t, string(minted), 1,
		"%d processes minted; the lock did not collapse them into one exchange", len(minted))
}

// A blocking flock cannot be interrupted, so a cancelled request would sit here
// until the holder finished.
func TestWaitingForTheLockHonoursTheContext(t *testing.T) {
	dir := t.TempDir()
	cache := newTestCache(t, dir)

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = cache.GetOrMint(context.Background(), func(context.Context) (string, time.Time, error) {
			close(held)
			<-release
			return "Bearer holder", time.Now().Add(oneHour), nil
		})
	}()
	<-held
	defer close(release)

	// A second Cache over the same paths contends for the same lock file, as a
	// second process would.
	waiter := newTestCache(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := waiter.GetOrMint(ctx, func(context.Context) (string, time.Time, error) {
		return "", time.Time{}, errors.New("the waiter should never have reached the mint")
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), time.Second, "the wait ignored the deadline")
}

// MkdirAll succeeds on a directory that already exists whatever its mode, so a
// world-writable one has to be refused: anything else on the host could
// pre-create the cache path.
func TestNewRefusesAWorldWritableDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0777))

	_, err := New(dir, "test", testFingerprint, testMargin, zap.NewNop())
	require.Error(t, err)
	require.Contains(t, err.Error(), "writable by group or other")
}

func TestNewAcceptsAPrivateDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))

	_, err := New(dir, "test", testFingerprint, testMargin, zap.NewNop())
	require.NoError(t, err)
}

func TestNewCreatesAPrivateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "created", "nested")

	_, err := New(dir, "test", testFingerprint, testMargin, zap.NewNop())
	require.NoError(t, err)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
}

func TestNewRejectsUnsafePathElements(t *testing.T) {
	dir := t.TempDir()

	_, err := New(dir, "../escape", testFingerprint, testMargin, zap.NewNop())
	require.Error(t, err)

	_, err = New(dir, "test", "../escape", testMargin, zap.NewNop())
	require.Error(t, err)
}

// A later change to the stored form has to stay readable by a running agent that
// is mid-refresh.
func TestStoredEntryShape(t *testing.T) {
	cache := newTestCache(t, t.TempDir())
	expiry := time.Now().Add(oneHour).Round(time.Second)

	_, err := cache.GetOrMint(context.Background(), func(context.Context) (string, time.Time, error) {
		return "Bearer shaped", expiry, nil
	})
	require.NoError(t, err)

	data, err := os.ReadFile(cache.Path())
	require.NoError(t, err)

	var stored entry
	require.NoError(t, json.Unmarshal(data, &stored))
	require.Equal(t, testFingerprint, stored.Fingerprint)
	require.Equal(t, "Bearer shaped", stored.Value)
	require.True(t, expiry.Equal(stored.Expiry))
}
