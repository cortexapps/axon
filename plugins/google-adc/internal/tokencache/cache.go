// Package tokencache persists a short-lived credential where a later process can
// find it.
//
// A credential provider that runs as a subprocess cannot keep a token in memory:
// the process that minted it has exited by the time the next request arrives.
// Without a cache that outlives the process, every request mints, which for a
// token the issuer intends to be reused is both slow and a self-inflicted rate
// limit.
//
// The cache also outlives the agent's own restarts, which an in-memory cache does
// not. The relay is rebuilt on its idle timeout, so an in-process token is
// discarded every few minutes on a link that is not busy.
//
// This is unix-only: it relies on flock, which the kernel releases when the
// holder exits. A lock that needed cleaning up after a crash would be worse than
// no lock at all.
package tokencache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// MintFunc produces a fresh credential and the moment it expires.
//
// A zero expiry means the issuer did not say, which this package treats as not
// cacheable rather than as valid forever.
type MintFunc func(ctx context.Context) (value string, expiry time.Time, err error)

// entry is the on-disk form.
//
// Fingerprint identifies which identity the value belongs to. It is checked on
// read, so a cache written under one configuration is never served under
// another - the failure that would otherwise happen silently, and look like an
// IAM problem on whichever identity lost the race.
type entry struct {
	Fingerprint string    `json:"fingerprint"`
	Value       string    `json:"value"`
	Expiry      time.Time `json:"expiry"`
}

// safeName bounds what can become part of a path, so a caller cannot turn a
// source name into a traversal.
var safeName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Cache stores one credential per (name, fingerprint) pair.
type Cache struct {
	path        string
	lockPath    string
	fingerprint string
	margin      time.Duration
	logger      *zap.Logger

	// now and pollInterval are seams for tests: expiry behaviour has to be
	// exercised without waiting for real time to pass.
	now          func() time.Time
	pollInterval time.Duration
}

// New prepares a cache under dir. The directory is created if missing, owner-only.
//
// margin is how long before expiry a value stops being served. It should match
// the refresh margin of whatever library mints the token, so the on-disk and
// in-memory views of "still usable" do not disagree.
func New(dir, name, fingerprint string, margin time.Duration, logger *zap.Logger) (*Cache, error) {
	if !safeName.MatchString(name) {
		return nil, fmt.Errorf("token cache name %q must match %s", name, safeName)
	}
	if !safeName.MatchString(fingerprint) {
		return nil, fmt.Errorf("token cache fingerprint for %q is not a safe path element", name)
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("cannot create token cache directory: %w", err)
	}
	if err := requirePrivateDir(dir); err != nil {
		return nil, err
	}

	base := filepath.Join(dir, name+"."+fingerprint)
	return &Cache{
		path:        base + ".json",
		lockPath:    base + ".lock",
		fingerprint: fingerprint,
		margin:      margin,
		logger:      logger.Named("token-cache"),
		now:         time.Now,
		// Short enough that a caller waiting on another process's mint is not
		// held past the exchange, long enough not to spin.
		pollInterval: 25 * time.Millisecond,
	}, nil
}

// Path reports the cache file's location, for diagnostics and tests.
func (c *Cache) Path() string { return c.path }

// requirePrivateDir refuses a directory anything else can write to.
//
// MkdirAll succeeds on a directory that already exists, whatever its mode or
// owner, so creating it privately is not the same as it being private. A
// world-writable parent lets another process pre-create the cache path, and the
// fingerprint check is then the only thing standing between that and a credential
// the agent would send upstream.
func requirePrivateDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("cannot inspect token cache directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("token cache path %s is not a directory", dir)
	}
	if mode := info.Mode().Perm(); mode&0022 != 0 {
		return fmt.Errorf("token cache directory %s is writable by group or other (mode %#o)", dir, mode)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Nothing to check against, rather than a reason to refuse to run.
		return nil
	}
	if uint64(stat.Uid) != uint64(os.Geteuid()) {
		return fmt.Errorf("token cache directory %s is owned by uid %d, not %d",
			dir, stat.Uid, os.Geteuid())
	}
	return nil
}

// GetOrMint returns a cached value, or mints and stores one.
//
// Concurrent cold callers - across processes, not just goroutines - collapse to a
// single mint: the lock is taken first and the cache re-read under it, so
// everyone except the first finds the value already there.
func (c *Cache) GetOrMint(ctx context.Context, mint MintFunc) (string, error) {
	// Unlocked read. The file is only ever replaced by rename, so a reader sees
	// either the whole previous value or the whole new one, and the common case
	// costs no lock.
	if value, ok := c.read(); ok {
		return value, nil
	}

	unlock, err := c.lock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()

	// Whoever held the lock was probably minting. This is the check that turns N
	// concurrent cold callers into one exchange.
	if value, ok := c.read(); ok {
		return value, nil
	}

	value, expiry, err := mint(ctx)
	if err != nil {
		return "", err
	}

	if err := c.write(entry{Fingerprint: c.fingerprint, Value: value, Expiry: expiry}); err != nil {
		// The token is good even though we could not keep it. Returning the
		// error here would turn a full disk into an outage; the cost of carrying
		// on is that the next process mints again.
		c.logger.Warn("Could not persist the credential, the next request will mint again",
			zap.String("path", c.path),
			zap.Error(err),
		)
	}
	return value, nil
}

// read returns a cached value that is still usable. Every failure is a miss:
// a cache that cannot be read or trusted is one to replace, not to report.
func (c *Cache) read() (string, bool) {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return "", false
	}

	var stored entry
	if err := json.Unmarshal(data, &stored); err != nil {
		return "", false
	}
	if stored.Value == "" || stored.Fingerprint != c.fingerprint {
		return "", false
	}
	// No expiry means no bound we could justify, so it was never written.
	if stored.Expiry.IsZero() {
		return "", false
	}
	if !c.now().Before(stored.Expiry.Add(-c.margin)) {
		return "", false
	}
	return stored.Value, true
}

// write replaces the cache file atomically, so a reader never observes a partial
// credential. A truncate-in-place would hand an empty string to whatever is
// building an Authorization header.
func (c *Cache) write(stored entry) error {
	if stored.Expiry.IsZero() {
		// Not an error: some credential types do not report an expiry, and those
		// are minted per process rather than cached without a bound.
		return nil
	}
	if !c.now().Before(stored.Expiry.Add(-c.margin)) {
		// Already inside the refresh margin, so read would reject it. Writing it
		// anyway would leave a credential on disk that nothing can ever use.
		return nil
	}

	data, err := json.Marshal(stored)
	if err != nil {
		return err
	}

	dir := filepath.Dir(c.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(c.path)+".*.tmp")
	if err != nil {
		return err
	}
	// Removes the temporary file on every path that does not reach the rename.
	defer os.Remove(tmp.Name())

	// Explicit rather than relying on CreateTemp's mode, because the content is a
	// credential and the mode is the only thing keeping it to this user.
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.path)
}

// lock takes an exclusive flock and returns the release.
//
// The lock file is created once and never renamed or removed: flock is held on an
// inode, so replacing the file would hand two processes locks on different
// inodes and neither would exclude the other. That is also why the lock is not
// the cache file, which is replaced on every write.
func (c *Cache) lock(ctx context.Context) (func(), error) {
	file, err := os.OpenFile(c.lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("cannot open token cache lock: %w", err)
	}

	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				// Best effort: the kernel releases the lock on close, and on
				// process exit if we never get here.
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			file.Close()
			return nil, fmt.Errorf("cannot lock token cache: %w", err)
		}

		// Polled rather than a blocking LOCK_EX, because a blocking flock cannot
		// be interrupted by a context: the caller's deadline would not apply to
		// the one case where waiting actually happens.
		select {
		case <-ctx.Done():
			file.Close()
			return nil, fmt.Errorf("waiting for the token cache lock: %w", ctx.Err())
		case <-time.After(c.pollInterval):
		}
	}
}
