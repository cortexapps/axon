// Package logstore captures the agent's own zap output in a size-bounded,
// segmented store on disk so __axon/logs can page through it remotely.
//
// The writer and the reader share one on-disk contract: each zap entry is a
// single JSON line with an epoch-milliseconds "ts" field, in files named
// axon-<seq>.log under one directory. Nothing outside this package should
// depend on that layout.
//
// The package never takes a *zap.Logger: it is a sink for the logger, and
// logging through it would recurse. Its own diagnostics go to stderr.
package logstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cortexapps/axon/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	// DefaultPageSize is what a request gets when it names no size.
	DefaultPageSize = 1000
	// MaxPageSize caps a single page so a request cannot ask for the whole store.
	MaxPageSize = 10000

	minSegmentBytes = int64(1 << 20)
	maxSegmentBytes = int64(8 << 20)
	filePrefix      = "axon-"
	fileSuffix      = ".log"

	// writeErrorNoticeInterval rate-limits the stderr notice for a store that
	// cannot write (disk full, directory removed). One line per failure would
	// be one line per log line.
	writeErrorNoticeInterval = time.Minute
)

// ErrDisabled is returned by Read when the store is not capturing.
var ErrDisabled = errors.New("remote logs are not enabled")

var segmentNameRe = regexp.MustCompile(`^axon-(\d+)\.log$`)

// Options configure an enabled store.
type Options struct {
	// Path is the directory segments live in. Created if missing.
	Path string
	// MaxSizeBytes bounds the total of all segments on disk.
	MaxSizeBytes int64
	// SegmentBytes is the size at which the active segment rolls. Zero
	// means a tenth of MaxSizeBytes, clamped to [1MB, 8MB] and never more
	// than half of MaxSizeBytes so at least two segments fit.
	SegmentBytes int64
	// Level is the enabler for the file core. Nil means InfoLevel.
	Level zapcore.LevelEnabler
}

// Store is a zapcore.WriteSyncer that appends to segmented files and prunes
// the oldest when the total exceeds its bound. A disabled Store is inert:
// writes succeed and go nowhere, Core is a no-op, Read reports ErrDisabled.
type Store struct {
	opts    Options
	enabled bool
	initErr error

	mu        sync.Mutex
	f         *os.File
	seq       uint64
	size      int64 // bytes in the active segment
	closed    bool
	lastErr   error
	lastErrAt time.Time
}

// LevelFor maps a REMOTE_LOGS_VISIBLE value to the file core's level. "*"
// is everything; a named level is that level and above; anything else,
// including empty, is info. Validation of the value lives in config.
func LevelFor(visible string) zapcore.Level {
	switch strings.ToLower(strings.TrimSpace(visible)) {
	case "*", "debug":
		return zapcore.DebugLevel
	case "warn":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}

// Disabled returns a store that captures nothing.
func Disabled() *Store {
	return &Store{}
}

// NewFromConfig builds the store the agent config asks for. It never fails:
// when capture is off it returns a disabled store, and when the configured
// path cannot be opened it returns a disabled store carrying the error, so
// a bad REMOTE_LOGS_PATH costs the diagnostics rather than the agent.
func NewFromConfig(cfg config.AgentConfig) *Store {
	if !cfg.RemoteLogsEnabled() {
		return Disabled()
	}
	s, err := Open(Options{
		Path:         cfg.RemoteLogsPath,
		MaxSizeBytes: cfg.RemoteLogsMaxSizeBytes,
		Level:        LevelFor(cfg.RemoteLogsVisible),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "axon: remote logs disabled: %v\n", err)
		d := Disabled()
		d.initErr = err
		return d
	}
	return s
}

// Open creates or resumes a store at opts.Path.
func Open(opts Options) (*Store, error) {
	if opts.Path == "" {
		return nil, errors.New("logstore: path is required")
	}
	if opts.MaxSizeBytes <= 0 {
		return nil, errors.New("logstore: max size must be positive")
	}
	if opts.SegmentBytes <= 0 {
		opts.SegmentBytes = defaultSegmentBytes(opts.MaxSizeBytes)
	}
	if opts.Level == nil {
		opts.Level = zapcore.InfoLevel
	}

	if err := os.MkdirAll(opts.Path, 0755); err != nil {
		return nil, fmt.Errorf("logstore: create %s: %w", opts.Path, err)
	}

	s := &Store{opts: opts, enabled: true}
	if err := s.resume(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s, s.pruneLocked()
}

func defaultSegmentBytes(maxSize int64) int64 {
	seg := maxSize / 10
	if seg < minSegmentBytes {
		seg = minSegmentBytes
	}
	if seg > maxSegmentBytes {
		seg = maxSegmentBytes
	}
	if seg > maxSize/2 {
		seg = maxSize / 2
	}
	if seg < 1 {
		seg = 1
	}
	return seg
}

// resume picks up the highest existing segment if it still has room, so a
// crash-looping agent does not leave thousands of tiny files behind that
// byte-based pruning would never remove. A torn trailing line from a crash
// gets a newline so the next entry starts clean; the reader skips the torn
// one because it does not parse.
func (s *Store) resume() error {
	segs, err := s.listSegments()
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		return s.openSegment(1)
	}
	newest := segs[0]
	if newest.size >= s.opts.SegmentBytes {
		return s.openSegment(newest.seq + 1)
	}
	if err := s.openSegment(newest.seq); err != nil {
		return err
	}
	if newest.size > 0 && !endsWithNewline(newest.path) {
		if _, err := s.f.Write([]byte{'\n'}); err != nil {
			return fmt.Errorf("logstore: repair %s: %w", newest.path, err)
		}
		s.size++
	}
	return nil
}

func endsWithNewline(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	buf := make([]byte, 1)
	if _, err := f.Seek(-1, 2); err != nil {
		return true
	}
	if _, err := f.Read(buf); err != nil {
		return true
	}
	return buf[0] == '\n'
}

func (s *Store) segmentPath(seq uint64) string {
	return filepath.Join(s.opts.Path, fmt.Sprintf("%s%d%s", filePrefix, seq, fileSuffix))
}

func parseSegmentName(name string) (uint64, bool) {
	m := segmentNameRe.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	seq, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return seq, true
}

// openSegment opens seq for append and makes it the active segment. The
// caller holds the lock or is still constructing the store.
func (s *Store) openSegment(seq uint64) error {
	path := s.segmentPath(seq)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("logstore: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("logstore: stat %s: %w", path, err)
	}
	if s.f != nil {
		s.f.Close()
	}
	s.f = f
	s.seq = seq
	s.size = info.Size()
	return nil
}

// Enabled reports whether the store captures. It is safe on a nil receiver
// so a handler built without a store can treat it as off.
func (s *Store) Enabled() bool {
	return s != nil && s.enabled
}

// Err is why a configured store ended up disabled, or nil.
func (s *Store) Err() error {
	if s == nil {
		return nil
	}
	return s.initErr
}

// ZapOption is what the agent adds to its logger so entries also land in
// the store. It tees the store's core next to whatever core the logger
// already has, so the logger's own level and encoder keep governing stderr.
// For a disabled or nil store it leaves the logger untouched, so the caller
// adds it unconditionally.
func (s *Store) ZapOption() zap.Option {
	return zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		if !s.Enabled() {
			return c
		}
		return zapcore.NewTee(c, s.core())
	})
}

// core carries its own JSON encoder: the on-disk format must not follow
// whatever ENV picks for stderr, because the reader depends on these key
// names.
func (s *Store) core() zapcore.Core {
	return zapcore.NewCore(fileEncoder(), s, s.opts.Level)
}

func fileEncoder() zapcore.Encoder {
	return zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		TimeKey:        "ts",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     zapcore.EpochMillisTimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	})
}

// Write appends one encoded entry. zap's core hands over each entry in a
// single call, so a line is never split across segments: the store rotates
// before the write if the line would not fit. Errors are swallowed on
// purpose; returning them would have zap print one notice per log line.
func (s *Store) Write(p []byte) (int, error) {
	if !s.Enabled() {
		return len(p), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return len(p), nil
	}

	if s.f == nil || (s.size > 0 && s.size+int64(len(p)) > s.opts.SegmentBytes) {
		if err := s.rotateLocked(); err != nil {
			s.noteErrorLocked(err)
			return len(p), nil
		}
	}

	n, err := s.f.Write(p)
	s.size += int64(n)
	if err != nil {
		s.noteErrorLocked(err)
	}
	return len(p), nil
}

// Sync flushes the active segment.
func (s *Store) Sync() error {
	if !s.Enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil || s.closed {
		return nil
	}
	return s.f.Sync()
}

// Close stops capture. Later writes are dropped silently: fx keeps logging
// after its OnStop hooks have run. Idempotent.
func (s *Store) Close() error {
	if !s.Enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

func (s *Store) rotateLocked() error {
	if err := s.openSegment(s.seq + 1); err != nil {
		return err
	}
	return s.pruneLocked()
}

// pruneLocked removes the oldest closed segments until the closed ones plus
// one full segment fit in the bound, so the on-disk peak is MaxSizeBytes
// rather than MaxSizeBytes plus a segment. The active segment is never
// removed.
func (s *Store) pruneLocked() error {
	segs, err := s.listSegments()
	if err != nil {
		return err
	}
	var closedTotal int64
	for _, seg := range segs {
		if seg.seq != s.seq {
			closedTotal += seg.size
		}
	}
	// segs is newest first; walk from the oldest.
	for i := len(segs) - 1; i >= 0 && closedTotal+s.opts.SegmentBytes > s.opts.MaxSizeBytes; i-- {
		seg := segs[i]
		if seg.seq == s.seq {
			continue
		}
		if err := os.Remove(seg.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("logstore: prune %s: %w", seg.path, err)
		}
		closedTotal -= seg.size
	}
	return nil
}

func (s *Store) noteErrorLocked(err error) {
	s.lastErr = err
	if time.Since(s.lastErrAt) < writeErrorNoticeInterval {
		return
	}
	s.lastErrAt = time.Now()
	fmt.Fprintf(os.Stderr, "axon: remote log store write failed: %v\n", err)
}

type segment struct {
	seq  uint64
	path string
	size int64
}

// listSegments returns the store's segments newest first. Files that do not
// match the naming contract are ignored.
func (s *Store) listSegments() ([]segment, error) {
	entries, err := os.ReadDir(s.opts.Path)
	if err != nil {
		return nil, fmt.Errorf("logstore: list %s: %w", s.opts.Path, err)
	}
	segs := make([]segment, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		seq, ok := parseSegmentName(e.Name())
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		segs = append(segs, segment{seq: seq, path: filepath.Join(s.opts.Path, e.Name()), size: info.Size()})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].seq > segs[j].seq })
	return segs, nil
}
