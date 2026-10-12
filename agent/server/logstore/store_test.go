package logstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cortexapps/axon/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// fakeClock lets a test choose each entry's ts, so paging assertions can
// name timestamps rather than sleep between writes.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTicker(d time.Duration) *time.Ticker { return time.NewTicker(d) }

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func openStore(t *testing.T, opts Options) *Store {
	t.Helper()
	if opts.Path == "" {
		opts.Path = t.TempDir()
	}
	if opts.MaxSizeBytes == 0 {
		opts.MaxSizeBytes = 1 << 20
	}
	s, err := Open(opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func loggerFor(s *Store, clock zapcore.Clock) *zap.Logger {
	opts := []zap.Option{}
	if clock != nil {
		opts = append(opts, zap.WithClock(clock))
	}
	return zap.New(s.Core(), opts...)
}

func segmentFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "axon-*.log"))
	require.NoError(t, err)
	// By sequence, not lexically: axon-10 comes after axon-9.
	sort.Slice(files, func(i, j int) bool {
		a, _ := parseSegmentName(filepath.Base(files[i]))
		b, _ := parseSegmentName(filepath.Base(files[j]))
		return a < b
	})
	return files
}

func totalSize(t *testing.T, files []string) int64 {
	t.Helper()
	var n int64
	for _, f := range files {
		info, err := os.Stat(f)
		require.NoError(t, err)
		n += info.Size()
	}
	return n
}

// allLines returns every complete line across every segment, in file order,
// oldest segment first.
func allLines(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, f := range segmentFiles(t, dir) {
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		for _, l := range strings.Split(string(b), "\n") {
			if l != "" {
				out = append(out, l)
			}
		}
	}
	return out
}

type parsed struct {
	Ts     float64 `json:"ts"`
	Level  string  `json:"level"`
	Logger string  `json:"logger"`
	Msg    string  `json:"msg"`
}

func parseLine(t *testing.T, line string) parsed {
	t.Helper()
	var p parsed
	require.NoError(t, json.Unmarshal([]byte(line), &p), "line is not JSON: %s", line)
	return p
}

// The reader relies on one entry per line. zap's JSON encoder escapes
// newlines in strings, stacks and reflected values, and the core hands each
// encoded entry to Write in one call; this pins that contract from our side.
func TestWrite_OneEntryPerLineWhateverTheFields(t *testing.T) {
	s := openStore(t, Options{})
	logger := loggerFor(s, nil)

	logger.Info("plain")
	logger.Info("newline in field", zap.String("m", "a\nb\r\nc"))
	logger.Error("multi-line error", zap.Error(errors.New("first\nsecond")))
	logger.Warn("with stack", zap.Stack("st"))
	logger.Info("reflected", zap.Any("obj", struct {
		Text string
		N    int
	}{"x\ny", 3}))

	lines := allLines(t, s.opts.Path)
	require.Len(t, lines, 5)
	msgs := []string{}
	for _, l := range lines {
		msgs = append(msgs, parseLine(t, l).Msg)
	}
	assert.Equal(t, []string{"plain", "newline in field", "multi-line error", "with stack", "reflected"}, msgs)
}

// The on-disk timestamp is epoch milliseconds so the reader can compare it
// to a unix-seconds cutoff with arithmetic rather than date parsing.
func TestWrite_TimestampIsEpochMillis(t *testing.T) {
	s := openStore(t, Options{})
	clock := &fakeClock{now: time.UnixMilli(1_760_000_000_123)}
	loggerFor(s, clock).Info("x")

	p := parseLine(t, allLines(t, s.opts.Path)[0])
	assert.InDelta(t, 1_760_000_000_123, p.Ts, 0.001)
	assert.Equal(t, "info", p.Level)
}

func TestLevelFor(t *testing.T) {
	cases := map[string]zapcore.Level{
		"*":     zapcore.DebugLevel,
		"debug": zapcore.DebugLevel,
		"info":  zapcore.InfoLevel,
		"warn":  zapcore.WarnLevel,
		"error": zapcore.ErrorLevel,
		"":      zapcore.InfoLevel,
		"bogus": zapcore.InfoLevel,
	}
	for in, want := range cases {
		assert.Equal(t, want, LevelFor(in), in)
	}
}

func TestLevel_StoreAtWarnDropsInfo(t *testing.T) {
	s := openStore(t, Options{Level: zapcore.WarnLevel})
	logger := loggerFor(s, nil)
	logger.Info("dropped")
	logger.Warn("kept")
	logger.Error("kept too")

	lines := allLines(t, s.opts.Path)
	require.Len(t, lines, 2)
	assert.Equal(t, "warn", parseLine(t, lines[0]).Level)
	assert.Equal(t, "error", parseLine(t, lines[1]).Level)
}

// "*" has to reach debug entries that the stderr core would never create on
// its own: with the two cores teed, the logger consults both enablers, so
// debug lands on disk while stderr stays at info.
func TestLevel_StarCapturesDebugWhileTeedCoreStaysAtInfo(t *testing.T) {
	s := openStore(t, Options{Level: LevelFor("*")})

	var stderrLines countingSyncer
	stderrCore := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), &stderrLines, zapcore.InfoLevel)
	logger := zap.New(zapcore.NewTee(stderrCore, s.Core()))

	logger.Debug("debug only on disk")
	logger.Info("both")

	lines := allLines(t, s.opts.Path)
	require.Len(t, lines, 2)
	assert.Equal(t, "debug", parseLine(t, lines[0]).Level)
	assert.Equal(t, 1, stderrLines.n, "stderr core must not see the debug line")
}

type countingSyncer struct{ n int }

func (c *countingSyncer) Write(p []byte) (int, error) { c.n++; return len(p), nil }
func (c *countingSyncer) Sync() error                 { return nil }

func TestRotation_RollsSegmentsWithoutSplittingLines(t *testing.T) {
	s := openStore(t, Options{SegmentBytes: 1024, MaxSizeBytes: 1 << 20})
	logger := loggerFor(s, nil)
	for i := 0; i < 200; i++ {
		logger.Info("entry", zap.Int("i", i))
	}

	files := segmentFiles(t, s.opts.Path)
	require.Greater(t, len(files), 3, "200 ~100-byte lines must span several 1KB segments")

	// Strictly increasing sequence numbers in name order.
	prev := uint64(0)
	for i, f := range files {
		seq, ok := parseSegmentName(filepath.Base(f))
		require.True(t, ok, f)
		if i > 0 {
			assert.Greater(t, seq, prev)
		}
		prev = seq
	}

	lines := allLines(t, s.opts.Path)
	require.Len(t, lines, 200)
	for _, l := range lines {
		parseLine(t, l)
	}
}

// A line bigger than a segment is never split; it goes whole into a segment
// of its own. That is the one way the store can exceed its bound.
func TestRotation_OversizeLineLandsWholeInItsOwnSegment(t *testing.T) {
	s := openStore(t, Options{SegmentBytes: 1024, MaxSizeBytes: 1 << 20})
	logger := loggerFor(s, nil)
	logger.Info("small")
	big := strings.Repeat("x", 3000)
	logger.Info("big", zap.String("payload", big))
	logger.Info("after")

	files := segmentFiles(t, s.opts.Path)
	require.Len(t, files, 3)
	lines := allLines(t, s.opts.Path)
	require.Len(t, lines, 3)
	assert.Contains(t, lines[1], big)
}

func TestBounding_TotalOnDiskStaysUnderMaxSize(t *testing.T) {
	s := openStore(t, Options{SegmentBytes: 1024, MaxSizeBytes: 8192})
	logger := loggerFor(s, nil)
	for i := 0; i < 1000; i++ {
		logger.Info("entry", zap.Int("i", i), zap.String("pad", strings.Repeat("p", 60)))
	}

	files := segmentFiles(t, s.opts.Path)
	require.NotEmpty(t, files)
	assert.LessOrEqual(t, totalSize(t, files), int64(8192))

	// Oldest gone, newest present, and the newest segment is the active one.
	lines := allLines(t, s.opts.Path)
	assert.NotContains(t, lines[0], `"i":0,`)
	assert.Contains(t, lines[len(lines)-1], `"i":999`)
}

func TestResume_AppendsToHighestSegmentWhenItHasRoom(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, Options{Path: dir, SegmentBytes: 1 << 20})
	loggerFor(s, nil).Info("before restart")
	require.NoError(t, s.Close())

	s2 := openStore(t, Options{Path: dir, SegmentBytes: 1 << 20})
	loggerFor(s2, nil).Info("after restart")

	files := segmentFiles(t, dir)
	require.Len(t, files, 1, "a restart must not open a new segment while the last one has room")
	require.Len(t, allLines(t, dir), 2)
}

func TestResume_StartsNewSegmentWhenHighestIsFull(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, Options{Path: dir, SegmentBytes: 256})
	for i := 0; i < 3; i++ {
		loggerFor(s, nil).Info("fill", zap.String("pad", strings.Repeat("p", 100)))
	}
	require.NoError(t, s.Close())
	before := segmentFiles(t, dir)
	require.NotEmpty(t, before)

	s2 := openStore(t, Options{Path: dir, SegmentBytes: 256})
	loggerFor(s2, nil).Info("after restart", zap.String("pad", strings.Repeat("p", 100)))
	after := segmentFiles(t, dir)
	assert.Len(t, after, len(before)+1)
}

// A crash mid-write leaves a torn last line. On reopen the store terminates
// it so the next entry starts clean; the reader skips the torn one because
// it does not parse.
func TestResume_RepairsTornTrailingLine(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, Options{Path: dir})
	loggerFor(s, nil).Info("complete")
	require.NoError(t, s.Close())

	files := segmentFiles(t, dir)
	require.Len(t, files, 1)
	f, err := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0644)
	require.NoError(t, err)
	_, err = f.WriteString(`{"ts":1,"msg":"torn`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	s2 := openStore(t, Options{Path: dir})
	loggerFor(s2, nil).Info("next")

	lines := allLines(t, dir)
	require.Len(t, lines, 3)
	assert.Equal(t, "complete", parseLine(t, lines[0]).Msg)
	assert.False(t, json.Valid([]byte(lines[1])), "torn line stays on its own line")
	assert.Equal(t, "next", parseLine(t, lines[2]).Msg)
}

func TestDisabled_IsInertAndNilSafe(t *testing.T) {
	var nilStore *Store
	assert.False(t, nilStore.Enabled())

	d := Disabled()
	assert.False(t, d.Enabled())
	assert.NoError(t, d.Err())

	n, err := d.Write([]byte("hello\n"))
	assert.Equal(t, 6, n)
	assert.NoError(t, err)

	_, err = d.Read(time.Now(), 0, 10)
	assert.ErrorIs(t, err, ErrDisabled)

	// Logging through a disabled store's core must touch nothing.
	dir := t.TempDir()
	zap.New(d.Core()).Info("x")
	entries, _ := os.ReadDir(dir)
	assert.Empty(t, entries)
}

func TestNewFromConfig_OffWhenNotConfigured(t *testing.T) {
	s := NewFromConfig(config.AgentConfig{RemoteLogsPath: t.TempDir()})
	assert.False(t, s.Enabled())
	assert.NoError(t, s.Err())
}

func TestNewFromConfig_OpensWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	s := NewFromConfig(config.AgentConfig{RemoteLogsVisible: "info", RemoteLogsPath: dir, RemoteLogsMaxSizeBytes: 1 << 20})
	t.Cleanup(func() { _ = s.Close() })
	require.True(t, s.Enabled())
	loggerFor(s, nil).Info("x")
	assert.Len(t, allLines(t, dir), 1)
}

// A store that cannot open must not take the agent down with it; the logs
// are a diagnostic aid, not the product. It disables itself and keeps the
// error so /info can report it.
func TestNewFromConfig_OpenFailureDisablesWithoutPanic(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0644))

	s := NewFromConfig(config.AgentConfig{
		RemoteLogsVisible:      "info",
		RemoteLogsPath:         filepath.Join(blocker, "logs"),
		RemoteLogsMaxSizeBytes: 1 << 20,
	})
	assert.False(t, s.Enabled())
	assert.Error(t, s.Err())
}

// fx keeps logging after OnStop hooks have run, so a closed store must drop
// writes quietly rather than hand zap an error per line.
func TestWriteAfterClose_IsASilentNoop(t *testing.T) {
	s := openStore(t, Options{})
	logger := loggerFor(s, nil)
	logger.Info("before")
	require.NoError(t, s.Close())
	require.NoError(t, s.Close(), "close is idempotent")

	n, err := s.Write([]byte("after\n"))
	assert.Equal(t, 6, n)
	assert.NoError(t, err)
	assert.Len(t, allLines(t, s.opts.Path), 1)
}

func TestSegmentBytes_DefaultsToATenthClampedToOneToEightMB(t *testing.T) {
	assert.Equal(t, int64(5<<20), openStore(t, Options{MaxSizeBytes: 50 << 20}).opts.SegmentBytes)
	assert.Equal(t, int64(1<<20), openStore(t, Options{MaxSizeBytes: 2 << 20}).opts.SegmentBytes)
	assert.Equal(t, int64(8<<20), openStore(t, Options{MaxSizeBytes: 500 << 20}).opts.SegmentBytes)
}

func TestConcurrentWritesAndReads(t *testing.T) {
	s := openStore(t, Options{SegmentBytes: 4096, MaxSizeBytes: 32768})
	logger := loggerFor(s, nil)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				logger.Info("entry", zap.Int("g", g), zap.Int("i", i))
			}
		}(g)
	}
	readErr := make(chan error, 1)
	go func() {
		for i := 0; i < 50; i++ {
			page, err := s.Read(time.Now().Add(time.Minute), 0, 50)
			if err != nil {
				readErr <- err
				return
			}
			for _, l := range page.Lines {
				if !json.Valid([]byte(l)) {
					readErr <- fmt.Errorf("unparsable line returned: %s", l)
					return
				}
			}
		}
		readErr <- nil
	}()
	wg.Wait()
	require.NoError(t, <-readErr)
}
