package logstore

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

// Page is one block of captured lines, chronological within the block.
// Read returns the newest block; Continue with Next returns the block
// just older than it, and Next is empty once nothing older is left.
type Page struct {
	Lines []string `json:"lines"`
	Next  string   `json:"next,omitempty"`
}

// ErrBadToken is returned by Continue for a token this store did not issue.
var ErrBadToken = errors.New("invalid continuation token")

// cursor is what a continuation token carries: the segment and the byte
// offset of the oldest line already returned. The next page is whatever
// ends at or before that offset in that segment, then older segments.
//
// A byte offset is what makes paging stable while the agent keeps logging:
// appends only ever land after it, or in a newer segment, so nothing behind
// a token moves. No timestamp is needed.
type cursor struct {
	seq    uint64
	offset int64
}

// tail is the cursor for a first page: newer than any segment, unbounded.
var tail = cursor{seq: math.MaxUint64, offset: math.MaxInt64}

func (c cursor) token() string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatUint(c.seq, 10) + ":" + strconv.FormatInt(c.offset, 10)))
}

func parseToken(token string) (cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return cursor{}, ErrBadToken
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 2 {
		return cursor{}, ErrBadToken
	}
	seq, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return cursor{}, ErrBadToken
	}
	offset, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || offset < 0 {
		return cursor{}, ErrBadToken
	}
	return cursor{seq: seq, offset: offset}, nil
}

// Read returns the newest `size` lines.
func (s *Store) Read(size int) (Page, error) {
	if !s.Enabled() {
		return Page{}, ErrDisabled
	}
	return s.read(tail, size)
}

// Continue returns the `size` lines just older than the page that issued
// `next`. A token from before pruning simply runs out of segments sooner.
func (s *Store) Continue(next string, size int) (Page, error) {
	if !s.Enabled() {
		return Page{}, ErrDisabled
	}
	c, err := parseToken(next)
	if err != nil {
		return Page{}, err
	}
	return s.read(c, size)
}

type lineRef struct {
	seq   uint64
	start int64
	text  string
}

// read walks segments newest first, taking from the tail of each until the
// page is full. It never takes the writer's lock. A segment that pruning
// removed in between is skipped: only the oldest are ever pruned, so what it
// held was beyond the end anyway. A trailing line without its newline is a
// write in progress and is dropped, as is any line that is not JSON; both
// rules are what make it safe to hand lines back verbatim.
func (s *Store) read(from cursor, size int) (Page, error) {
	if size < 1 {
		return Page{}, errors.New("size must be >= 1")
	}
	segs, err := s.listSegments()
	if err != nil {
		return Page{}, err
	}

	want := size + 1 // one extra proves an older line exists
	var chunks [][]lineRef
	for _, seg := range segs {
		if want == 0 {
			break
		}
		if seg.seq > from.seq {
			continue
		}
		limit := int64(math.MaxInt64)
		if seg.seq == from.seq {
			limit = from.offset
		}
		lines, err := readLines(seg, limit)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Page{}, err
		}
		start := len(lines) - want
		if start < 0 {
			start = 0
		}
		chunks = append(chunks, lines[start:])
		want -= len(lines) - start
	}

	// chunks were collected newest segment first; the page reads oldest first.
	var all []lineRef
	for i := len(chunks) - 1; i >= 0; i-- {
		all = append(all, chunks[i]...)
	}
	page := Page{Lines: make([]string, 0, len(all))}
	if len(all) > size {
		all = all[len(all)-size:]
		page.Next = cursor{seq: all[0].seq, offset: all[0].start}.token()
	}
	for _, l := range all {
		page.Lines = append(page.Lines, l.text)
	}
	return page, nil
}

// readLines returns, in file order, every complete JSON line in seg that
// ends at or before limit, with the offset it starts at. Memory is bounded
// by one segment.
func readLines(seg segment, limit int64) ([]lineRef, error) {
	f, err := os.Open(seg.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []lineRef
	var pos int64
	r := bufio.NewReaderSize(f, 64*1024)
	for pos < limit {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				// No newline: a write in progress or a torn line. Drop it.
				return out, nil
			}
			return out, fmt.Errorf("logstore: read %s: %w", seg.path, err)
		}
		start := pos
		pos += int64(len(line))
		if pos > limit {
			break
		}
		line = line[:len(line)-1]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if !json.Valid(line) {
			continue
		}
		out = append(out, lineRef{seq: seg.seq, start: start, text: string(line)})
	}
	return out, nil
}
