// Package memlimit derives GOMEMLIMIT from the container's own cgroup memory
// limit, so that sizing the container is the only dial an operator needs.
//
// Go does not read cgroup limits: left alone, the runtime sizes the heap from
// GOGC (2x live heap) and returns pages to the OS lazily, so a process whose
// live set is small but whose allocation rate is high will let RSS ratchet up
// toward the container limit and stay there. Telling the runtime what its
// budget actually is turns that ratchet into ordinary GC pressure.
//
// GOMEMLIMIT is a soft limit. The runtime never refuses an allocation because
// of it, so setting it cannot cause an OOM kill that would not have happened
// anyway; it can only make the collector work harder first. Go caps GC at 50%
// of CPU, so a live set that genuinely exceeds the budget overshoots the limit
// rather than thrashing forever.
package memlimit

import (
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

const (
	// headroomFraction is the share of the container's memory the Go heap may
	// target. The remainder covers what GOMEMLIMIT does not account for: the
	// binary itself, thread stacks, and any cgo or OS-level allocation.
	headroomFraction = 0.8

	// minLimitBytes guards against sizing the heap so small that the agent
	// spends all its time collecting. Below this we leave the runtime alone.
	minLimitBytes = 64 << 20

	cgroupV2Path = "/sys/fs/cgroup/memory.max"
	cgroupV1Path = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
)

// Configure sets GOMEMLIMIT from the cgroup memory limit and reports what it
// did, for logging. An explicit GOMEMLIMIT in the environment always wins: the
// runtime has already applied it, and an operator who set it meant it.
func Configure() string {
	if v := os.Getenv("GOMEMLIMIT"); v != "" {
		return fmt.Sprintf("GOMEMLIMIT=%s set explicitly; leaving runtime limit alone", v)
	}

	limit, source, ok := detectCgroupLimit(cgroupPaths)
	if !ok {
		return "no cgroup memory limit detected; leaving GOMEMLIMIT unset"
	}

	target := int64(float64(limit) * headroomFraction)
	if target < minLimitBytes {
		return fmt.Sprintf("cgroup limit %s (%s) too small to target safely; leaving GOMEMLIMIT unset",
			humanize(limit), source)
	}

	debug.SetMemoryLimit(target)
	return fmt.Sprintf("GOMEMLIMIT set to %s (%.0f%% of %s cgroup limit %s)",
		humanize(target), headroomFraction*100, source, humanize(limit))
}

// detectCgroupLimit reads the memory limit from cgroup v2, then v1. Both report
// "no limit" with a sentinel rather than an absent file, and the v1 sentinel is
// a very large number rather than a word, so treat anything implausibly large
// as unlimited.
type cgroupSource struct {
	path   string
	source string
}

var cgroupPaths = []cgroupSource{
	{cgroupV2Path, "v2"},
	{cgroupV1Path, "v1"},
}

func detectCgroupLimit(sources []cgroupSource) (int64, string, bool) {
	for _, c := range sources {
		raw, err := os.ReadFile(c.path)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(raw))
		if text == "max" {
			continue
		}
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		// cgroup v1 reports "unlimited" as a value near the pointer maximum.
		if n >= math.MaxInt64/2 {
			continue
		}
		return n, c.source, true
	}
	return 0, "", false
}

func humanize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 3; n /= unit {
		div *= unit
		exp++
	}
	// One decimal place, trimmed when it is zero: this string is how an
	// operator learns what budget the agent picked, and rounding 1.6GiB to
	// "2GiB" reports a number above the container limit it was derived from.
	v := float64(b) / float64(div)
	text := strconv.FormatFloat(v, 'f', 1, 64)
	text = strings.TrimSuffix(text, ".0")
	return fmt.Sprintf("%s%ciB", text, "KMGT"[exp])
}
