package logstore

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// 25 lines with ts = 1s..25s, padded so they span several segments.
func seedPaged(t *testing.T, segmentBytes int64) (*Store, *fakeClock) {
	t.Helper()
	s := openStore(t, Options{SegmentBytes: segmentBytes, MaxSizeBytes: 1 << 20})
	clock := &fakeClock{}
	logger := loggerFor(s, clock)
	for i := 1; i <= 25; i++ {
		clock.Set(time.Unix(int64(i), 0))
		logger.Info("entry", zap.Int("i", i))
	}
	require.Greater(t, len(segmentFiles(t, s.opts.Path)), 2, "seed must span segments for the walk to matter")
	return s, clock
}

func tsSeconds(t *testing.T, page Page) []int {
	t.Helper()
	out := make([]int, 0, len(page.Lines))
	for _, l := range page.Lines {
		out = append(out, int(parseLine(t, l).Ts/1000))
	}
	return out
}

func seq(from, to int) []int {
	out := []int{}
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

// walk pages from the newest block to the oldest and returns each page's
// ts, newest page first.
func walk(t *testing.T, s *Store, size int) [][]int {
	t.Helper()
	var pages [][]int
	page, err := s.Read(size)
	require.NoError(t, err)
	for {
		pages = append(pages, tsSeconds(t, page))
		if page.Next == "" {
			return pages
		}
		require.Len(t, page.Lines, size, "only the last page may be short")
		page, err = s.Continue(page.Next, size)
		require.NoError(t, err)
	}
}

func TestRead_FirstPageIsTheNewestBlockChronological(t *testing.T) {
	s, _ := seedPaged(t, 400)
	page, err := s.Read(10)
	require.NoError(t, err)
	assert.Equal(t, seq(16, 25), tsSeconds(t, page))
	assert.NotEmpty(t, page.Next, "older lines exist, so there is a continuation")
}

func TestRead_ContinueWalksBackToTheOldestLine(t *testing.T) {
	s, _ := seedPaged(t, 400)
	assert.Equal(t, [][]int{seq(16, 25), seq(6, 15), seq(1, 5)}, walk(t, s, 10))
}

func TestRead_ExactFitHasNoContinuation(t *testing.T) {
	s, _ := seedPaged(t, 400)
	page, err := s.Read(25)
	require.NoError(t, err)
	assert.Equal(t, seq(1, 25), tsSeconds(t, page))
	assert.Empty(t, page.Next, "the size+1 peek must not leak a token when nothing is older")
}

func TestRead_SizeLargerThanStoreReturnsEverything(t *testing.T) {
	s, _ := seedPaged(t, 400)
	page, err := s.Read(1000)
	require.NoError(t, err)
	assert.Equal(t, seq(1, 25), tsSeconds(t, page))
	assert.Empty(t, page.Next)
}

// Page boundaries that fall exactly on a segment boundary are the case a
// per-segment walk gets wrong by one. Small segments and sizes hit them.
func TestRead_PageBoundariesAcrossSegments(t *testing.T) {
	for _, size := range []int{1, 3, 7} {
		s, _ := seedPaged(t, 200)
		seen := []int{}
		for _, p := range walk(t, s, size) {
			seen = append(p, seen...)
		}
		assert.Equal(t, seq(1, 25), seen, "size %d", size)
	}
}

// The token pins the walk: lines logged after the first page was read never
// shift the pages behind it, and a fresh first page sees them.
func TestRead_ContinuationIsStableWhileTheAgentKeepsLogging(t *testing.T) {
	s, clock := seedPaged(t, 400)
	first, err := s.Read(5)
	require.NoError(t, err)
	assert.Equal(t, seq(21, 25), tsSeconds(t, first))

	logger := loggerFor(s, clock)
	for i := 26; i <= 30; i++ {
		clock.Set(time.Unix(int64(i), 0))
		logger.Info("entry", zap.Int("i", i))
	}

	second, err := s.Continue(first.Next, 5)
	require.NoError(t, err)
	assert.Equal(t, seq(16, 20), tsSeconds(t, second))

	fresh, err := s.Read(5)
	require.NoError(t, err)
	assert.Equal(t, seq(26, 30), tsSeconds(t, fresh))
}

func TestRead_RejectsBadToken(t *testing.T) {
	s, _ := seedPaged(t, 400)
	enc := func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
	for _, tok := range []string{"", "not base64!", enc("1"), enc("1:2:3"), enc("a:b"), enc("-1:0"), enc("1:-5")} {
		_, err := s.Continue(tok, 10)
		assert.ErrorIs(t, err, ErrBadToken, "token %q", tok)
	}
}

func TestRead_RejectsBadSize(t *testing.T) {
	s, _ := seedPaged(t, 400)
	_, err := s.Read(0)
	assert.Error(t, err)
	page, err := s.Read(10)
	require.NoError(t, err)
	_, err = s.Continue(page.Next, -1)
	assert.Error(t, err)
}

func TestRead_SkipsPartialTrailingLineUntilTerminated(t *testing.T) {
	s := openStore(t, Options{})
	loggerFor(s, nil).Info("whole")
	files := segmentFiles(t, s.opts.Path)
	require.Len(t, files, 1)

	f, err := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0644)
	require.NoError(t, err)
	_, err = f.WriteString(`{"ts":99,"msg":"partial"`)
	require.NoError(t, err)

	page, err := s.Read(10)
	require.NoError(t, err)
	require.Len(t, page.Lines, 1)

	_, err = f.WriteString("}\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	page, err = s.Read(10)
	require.NoError(t, err)
	require.Len(t, page.Lines, 2)
	assert.Equal(t, "partial", parseLine(t, page.Lines[1]).Msg)
}

func TestRead_SkipsLinesThatAreNotJSON(t *testing.T) {
	s := openStore(t, Options{})
	loggerFor(s, nil).Info("good")
	files := segmentFiles(t, s.opts.Path)
	f, err := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0644)
	require.NoError(t, err)
	_, err = f.WriteString("garbage line\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	loggerFor(s, nil).Info("good too")

	page, err := s.Read(10)
	require.NoError(t, err)
	require.Len(t, page.Lines, 2)
	assert.Equal(t, "good", parseLine(t, page.Lines[0]).Msg)
	assert.Equal(t, "good too", parseLine(t, page.Lines[1]).Msg)
}

// Pruning can remove a segment between the reader listing the directory and
// opening it. Only the oldest segments are ever pruned, so skipping is the
// right answer, not an error.
func TestRead_ToleratesSegmentRemovedMidWalk(t *testing.T) {
	s, _ := seedPaged(t, 200)
	files := segmentFiles(t, s.opts.Path)
	require.NoError(t, os.Remove(files[0]))
	require.NoError(t, os.Remove(files[1]))

	page, err := s.Read(25)
	require.NoError(t, err)
	assert.Less(t, len(page.Lines), 25)
	assert.Equal(t, 25, tsSeconds(t, page)[len(page.Lines)-1])
}

// A token handed out before pruning may point past what is left. The walk
// ends cleanly on what remains rather than failing.
func TestRead_ContinueAfterPruningEndsCleanly(t *testing.T) {
	s, _ := seedPaged(t, 200)
	page, err := s.Read(3)
	require.NoError(t, err)
	require.NotEmpty(t, page.Next)

	files := segmentFiles(t, s.opts.Path)
	for _, f := range files[:len(files)-1] {
		require.NoError(t, os.Remove(f))
	}

	for page.Next != "" {
		page, err = s.Continue(page.Next, 3)
		require.NoError(t, err)
	}
}

func TestRead_EmptyStoreReturnsEmptyPage(t *testing.T) {
	s := openStore(t, Options{})
	page, err := s.Read(10)
	require.NoError(t, err)
	assert.Equal(t, []string{}, page.Lines)
	assert.Empty(t, page.Next)
}

func TestListSegments_IgnoresForeignFiles(t *testing.T) {
	s := openStore(t, Options{})
	loggerFor(s, nil).Info("x")
	require.NoError(t, os.WriteFile(filepath.Join(s.opts.Path, "notes.txt"), []byte("hi"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(s.opts.Path, "axon-abc.log"), []byte("hi"), 0644))

	segs, err := s.listSegments()
	require.NoError(t, err)
	require.Len(t, segs, 1)
}
