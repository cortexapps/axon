// Package tokencache persists a short-lived credential where a later process can
// find it.
//
// A credential provider that runs as a subprocess cannot keep a token in memory:
// the process that minted it has exited by the time the next request arrives.
// The cache also outlives the agent's own restarts, which rebuilds the relay on
// its idle timeout and would otherwise discard an in-memory token every few
// minutes on a link that is not busy.
//
// This is unix-only: it relies on flock, which the kernel releases when the holder
// exits. A lock that needed cleaning up after a crash would be worse than no lock.
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

// MintFunc produces a fresh credential and the moment it expires. A zero expiry
// means the issuer did not say, which this package treats as not cacheable rather
// than as valid forever.
type MintFunc func(ctx context.Context) (value string, expiry time.Time, err error)

// entry is the on-disk form. Fingerprint is checked on read, so a cache written
// under one identity is never served under another.
type entry struct {
	Fingerprint string    `json:"fingerprint"`
	Value       string    `json:"value"`
	Expiry      time.Time `json:"expiry"`
}

// safeName bounds what can become part of a path, so a caller cannot turn a source
// name into a traversal.
var safeName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Cache stores one credential per (name, fingerprint) pair.
type Cache struct {
	path          string
	lockPath      string
	fingerprint   string
	refreshMargin time.Duration
	logger        *zap.Logger

	now          func() time.Time
	pollInterval time.Duration
}

// New prepares a cache under dir, creating the directory owner-only if missing.
//
// refreshMargin should match the refresh margin of whatever library mints the
// token, so the on-disk and in-memory views of "still usable" do not disagree.
func New(dir, name, fingerprint string, refreshMargin time.Duration, logger *zap.Logger) (*Cache, error) {
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
		path:          base + ".json",
		lockPath:      base + ".lock",
		fingerprint:   fingerprint,
		refreshMargin: refreshMargin,
		logger:        logger.Named("token-cache"),
		now:           time.Now,
		pollInterval:  25 * time.Millisecond,
	}, nil
}

func (c *Cache) Path() string { return c.path }

// requirePrivateDir refuses a directory anything else can write to. MkdirAll
// succeeds on a directory that already exists whatever its mode or owner, so
// creating it privately is not the same as it being private.
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
		return nil
	}
	if uint64(stat.Uid) != uint64(os.Geteuid()) {
		return fmt.Errorf("token cache directory %s is owned by uid %d, not %d",
			dir, stat.Uid, os.Geteuid())
	}
	return nil
}

// GetOrMint returns a cached value, or mints and stores one. Concurrent cold
// callers - across processes, not just goroutines - collapse to a single mint.
func (c *Cache) GetOrMint(ctx context.Context, mint MintFunc) (string, error) {
	// Unlocked. The file is only ever replaced by rename, so a reader sees either
	// the whole previous value or the whole new one.
	if value, ok := c.read(); ok {
		return value, nil
	}

	unlock, err := c.lock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()

	// Whoever held the lock was probably minting.
	if value, ok := c.read(); ok {
		return value, nil
	}

	value, expiry, err := mint(ctx)
	if err != nil {
		return "", err
	}

	if err := c.write(entry{Fingerprint: c.fingerprint, Value: value, Expiry: expiry}); err != nil {
		// The token is good even though we could not keep it. Failing here would
		// turn a full disk into an outage.
		c.logger.Warn("Could not persist the credential, the next request will mint again",
			zap.String("path", c.path),
			zap.Error(err),
		)
	}
	return value, nil
}

// read treats every failure as a miss: a cache that cannot be read or trusted is
// one to replace, not to report.
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
	if stored.Expiry.IsZero() || !c.usable(stored.Expiry) {
		return "", false
	}
	return stored.Value, true
}

// write replaces the cache file atomically, so a reader never observes a partial
// credential.
func (c *Cache) write(stored entry) error {
	// Nothing read would later serve is worth leaving on disk.
	if stored.Expiry.IsZero() || !c.usable(stored.Expiry) {
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

func (c *Cache) usable(expiry time.Time) bool {
	return c.now().Before(expiry.Add(-c.refreshMargin))
}

// lock takes an exclusive flock and returns the release.
//
// The lock file is created once and never renamed or removed: flock is held on an
// inode, so replacing the file would hand two processes locks on different inodes.
// That is also why the lock is not the cache file, which is replaced on every write.
func (c *Cache) lock(ctx context.Context) (func(), error) {
	file, err := os.OpenFile(c.lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("cannot open token cache lock: %w", err)
	}

	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			file.Close()
			return nil, fmt.Errorf("cannot lock token cache: %w", err)
		}

		// Polled rather than a blocking LOCK_EX, which cannot be interrupted by a
		// context: the caller's deadline would not apply to the one case where
		// waiting actually happens.
		select {
		case <-ctx.Done():
			file.Close()
			return nil, fmt.Errorf("waiting for the token cache lock: %w", ctx.Err())
		case <-time.After(c.pollInterval):
		}
	}
}
