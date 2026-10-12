package logstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"time"
)

// Page is one page of captured lines, newest page first across pages and
// chronological within a page.
type Page struct {
	// Now is the unix-seconds cutoff the page was computed against. A
	// client pins it on the next request so pages do not shift as the agent
	// keeps logging.
	Now     int64    `json:"now"`
	Page    int      `json:"page"`
	Size    int      `json:"size"`
	Count   int      `json:"count"`
	HasMore bool     `json:"has_more"`
	Lines   []string `json:"lines"`
}

// Read returns page `page` of size `size`, counting back from the newest
// line stamped at or before `now`. Page 0 is the newest block.
//
// The reader never takes the writer's lock. It lists segments once, newest
// first, and walks each from the tail. A segment that pruning removed in
// between is skipped: only the oldest are ever pruned, so what it held was
// beyond the end anyway. A trailing line without its newline is a write in
// progress and is dropped, as is any line that does not parse; both rules
// are what make it safe to hand lines back verbatim.
func (s *Store) Read(now time.Time, page, size int) (Page, error) {
	if !s.Enabled() {
		return Page{}, ErrDisabled
	}
	if page < 0 {
		return Page{}, errors.New("page must be >= 0")
	}
	if size < 1 {
		return Page{}, errors.New("size must be >= 1")
	}
	if page > 0 && size > math.MaxInt32/page {
		return Page{}, errors.New("page*size is too large")
	}

	result := Page{Now: now.Unix(), Page: page, Size: size, Lines: []string{}}
	// Computed the way zap's EpochMillisTimeEncoder stamps ts, fractional
	// milliseconds included, so "at or before now" holds to the precision
	// the entries carry rather than to a truncated millisecond.
	cutoffMs := float64(now.UnixNano()) / float64(time.Millisecond)

	segs, err := s.listSegments()
	if err != nil {
		return Page{}, err
	}

	skip := page * size
	want := size + 1 // one extra tells us whether another page exists
	var chunks [][]string
	for _, seg := range segs {
		if want == 0 {
			break
		}
		lines, err := readMatching(seg.path, cutoffMs)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Page{}, err
		}
		if len(lines) <= skip {
			skip -= len(lines)
			continue
		}
		end := len(lines) - skip
		start := end - want
		if start < 0 {
			start = 0
		}
		chunks = append(chunks, lines[start:end])
		want -= end - start
		skip = 0
	}

	// chunks were collected newest segment first; the page reads oldest first.
	for i := len(chunks) - 1; i >= 0; i-- {
		result.Lines = append(result.Lines, chunks[i]...)
	}
	if len(result.Lines) > size {
		result.Lines = result.Lines[len(result.Lines)-size:]
		result.HasMore = true
	}
	result.Count = len(result.Lines)
	return result, nil
}

// readMatching returns, in file order, every complete line in path whose ts
// is at or before cutoffMs. Memory is bounded by one segment.
func readMatching(path string, cutoffMs float64) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				// No newline: a write in progress or a torn line. Drop it.
				return out, nil
			}
			return out, err
		}
		line = line[:len(line)-1]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		var stamp struct {
			Ts float64 `json:"ts"`
		}
		if json.Unmarshal(line, &stamp) != nil {
			continue
		}
		if stamp.Ts > cutoffMs {
			continue
		}
		out = append(out, string(line))
	}
}
