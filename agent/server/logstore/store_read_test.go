package logstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// 25 lines with ts = 1s..25s, padded so they span several segments.
func seedPaged(t *testing.T, segmentBytes int64) *Store {
	t.Helper()
	s := openStore(t, Options{SegmentBytes: segmentBytes, MaxSizeBytes: 1 << 20})
	clock := &fakeClock{}
	logger := loggerFor(s, clock)
	for i := 1; i <= 25; i++ {
		clock.Set(time.Unix(int64(i), 0))
		logger.Info("entry", zap.Int("i", i))
	}
	require.Greater(t, len(segmentFiles(t, s.opts.Path)), 2, "seed must span segments for the walk to matter")
	return s
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

func TestRead_Paging(t *testing.T) {
	s := seedPaged(t, 400)

	cases := []struct {
		name    string
		now     int64
		page    int
		size    int
		want    []int
		hasMore bool
	}{
		{"page 0 is the newest block, chronological", 25, 0, 10, seq(16, 25), true},
		{"page 1 is the next older block", 25, 1, 10, seq(6, 15), true},
		{"last page is short with no more", 25, 2, 10, seq(1, 5), false},
		{"page past the end is empty", 25, 3, 10, []int{}, false},
		{"now excludes later lines", 10, 0, 10, seq(1, 10), false},
		{"now pins mid-stream with more behind it", 12, 0, 5, seq(8, 12), true},
		{"size 1 at the last page", 25, 24, 1, []int{1}, false},
		{"exact fit has no more", 25, 0, 25, seq(1, 25), false},
		{"now before first line", 0, 0, 10, []int{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			page, err := s.Read(time.Unix(c.now, 0), c.page, c.size)
			require.NoError(t, err)
			assert.Equal(t, c.want, tsSeconds(t, page))
			assert.Equal(t, c.hasMore, page.HasMore)
			assert.Equal(t, len(c.want), page.Count)
			assert.Equal(t, c.page, page.Page)
			assert.Equal(t, c.size, page.Size)
			assert.Equal(t, c.now, page.Now)
			assert.NotNil(t, page.Lines, "lines is always an array, never null")
		})
	}
}

// Page boundaries that fall exactly on a segment boundary are the case a
// per-segment walk gets wrong by one. Small segments and sizes hit them.
func TestRead_PageBoundariesAcrossSegments(t *testing.T) {
	s := seedPaged(t, 200)
	seen := []int{}
	for p := 0; ; p++ {
		page, err := s.Read(time.Unix(25, 0), p, 3)
		require.NoError(t, err)
		seen = append(tsSeconds(t, page), seen...)
		if !page.HasMore {
			break
		}
		require.Len(t, page.Lines, 3)
	}
	assert.Equal(t, seq(1, 25), seen)
}

// The cutoff is milliseconds: a line stamped inside the cutoff second is
// included, a line stamped 1ms into the next second is not.
func TestRead_CutoffIsInclusiveToTheMillisecond(t *testing.T) {
	s := openStore(t, Options{})
	clock := &fakeClock{}
	logger := loggerFor(s, clock)
	clock.Set(time.UnixMilli(10_999))
	logger.Info("in")
	clock.Set(time.UnixMilli(11_001))
	logger.Info("out")

	page, err := s.Read(time.Unix(11, 0), 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Lines, 1)
	assert.Equal(t, "in", parseLine(t, page.Lines[0]).Msg)
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

	page, err := s.Read(time.Now(), 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Lines, 1)

	_, err = f.WriteString("}\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	page, err = s.Read(time.Now(), 0, 10)
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

	page, err := s.Read(time.Now(), 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Lines, 2)
}

// Pruning can remove a segment between the reader listing the directory and
// opening it. Only the oldest segments are ever pruned, so skipping is the
// right answer, not an error.
func TestRead_ToleratesSegmentRemovedMidWalk(t *testing.T) {
	s := seedPaged(t, 200)
	files := segmentFiles(t, s.opts.Path)
	require.NoError(t, os.Remove(files[0]))
	require.NoError(t, os.Remove(files[1]))

	page, err := s.Read(time.Unix(25, 0), 0, 25)
	require.NoError(t, err)
	assert.Less(t, len(page.Lines), 25)
	assert.Equal(t, 25, tsSeconds(t, page)[len(page.Lines)-1])
}

func TestRead_EmptyStoreReturnsEmptyPage(t *testing.T) {
	s := openStore(t, Options{})
	page, err := s.Read(time.Now(), 0, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{}, page.Lines)
	assert.False(t, page.HasMore)
}

func TestRead_RejectsBadArguments(t *testing.T) {
	s := openStore(t, Options{})
	_, err := s.Read(time.Now(), -1, 10)
	assert.Error(t, err)
	_, err = s.Read(time.Now(), 0, 0)
	assert.Error(t, err)
	_, err = s.Read(time.Now(), 1<<40, 1<<40)
	assert.Error(t, err, "page*size overflow")
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
