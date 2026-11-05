package memlimit

import (
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
)

// write creates a fake cgroup file and returns a source list pointing at it.
func write(t *testing.T, source, contents string) []cgroupSource {
	t.Helper()
	path := filepath.Join(t.TempDir(), "limit")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fake cgroup file: %v", err)
	}
	return []cgroupSource{{path, source}}
}

func TestDetectCgroupLimit(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     int64
		wantOK   bool
	}{
		{"v2 byte count", "536870912\n", 536870912, true},
		{"no trailing newline", "268435456", 268435456, true},
		{"v2 unlimited sentinel", "max\n", 0, false},
		{"v1 unlimited sentinel", strconv.FormatInt(math.MaxInt64, 10), 0, false},
		{"garbage", "not-a-number\n", 0, false},
		{"zero", "0\n", 0, false},
		{"negative", "-1\n", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, ok := detectCgroupLimit(write(t, "v2", tt.contents))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("limit = %d, want %d", got, tt.want)
			}
		})
	}
}

// A missing cgroup file is the normal case off-container; it must not panic or
// claim a limit.
func TestDetectCgroupLimitMissingFile(t *testing.T) {
	sources := []cgroupSource{{filepath.Join(t.TempDir(), "absent"), "v2"}}
	if _, _, ok := detectCgroupLimit(sources); ok {
		t.Fatal("reported a limit from a nonexistent file")
	}
}

// v2 is consulted first, but an unreadable or unlimited v2 must fall through to
// v1 rather than giving up.
func TestDetectCgroupLimitFallsThroughToV1(t *testing.T) {
	v2 := filepath.Join(t.TempDir(), "v2")
	if err := os.WriteFile(v2, []byte("max\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v1 := filepath.Join(t.TempDir(), "v1")
	if err := os.WriteFile(v1, []byte("268435456\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, source, ok := detectCgroupLimit([]cgroupSource{{v2, "v2"}, {v1, "v1"}})
	if !ok {
		t.Fatal("expected the v1 limit to be found")
	}
	if got != 268435456 {
		t.Errorf("limit = %d, want 268435456", got)
	}
	if source != "v1" {
		t.Errorf("source = %q, want v1", source)
	}
}

// An operator who sets GOMEMLIMIT themselves has already told the runtime what
// they want; deriving one from the cgroup would silently override them.
func TestConfigureRespectsExplicitEnv(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "123MiB")
	if got := Configure(); got == "" {
		t.Fatal("expected a description of what Configure did")
	} else if !strings.Contains(got, "explicitly") {
		t.Errorf("Configure() = %q, want it to defer to the explicit setting", got)
	}
}

func TestHumanize(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{512, "512B"},
		{1 << 20, "1MiB"},
		{400 << 20, "400MiB"},
		{2 << 30, "2GiB"},
		{1717986918, "1.6GiB"},
		{410 << 20, "410MiB"},
	}
	for _, tt := range tests {
		if got := humanize(tt.in); got != tt.want {
			t.Errorf("humanize(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Configure must actually move the runtime's limit, not merely report that it
// would: the whole point is that the container's size reaches the collector.
func TestConfigureAppliesRuntimeLimit(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "")

	const containerLimit = 512 << 20
	restore := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(restore) })

	saved := cgroupPaths
	cgroupPaths = write(t, "v2", strconv.Itoa(containerLimit))
	t.Cleanup(func() { cgroupPaths = saved })

	msg := Configure()

	got := debug.SetMemoryLimit(-1)
	var limit int64 = containerLimit
	want := int64(float64(limit) * headroomFraction)
	if got != want {
		t.Errorf("runtime memory limit = %d, want %d (msg: %s)", got, want, msg)
	}
	if got >= containerLimit {
		t.Errorf("limit %d leaves no headroom under the container limit %d", got, containerLimit)
	}
}

// A container with no memory limit must leave the runtime at its default rather
// than inventing a budget.
func TestConfigureLeavesRuntimeAloneWithoutCgroupLimit(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "")

	before := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(before) })

	saved := cgroupPaths
	cgroupPaths = write(t, "v2", "max")
	t.Cleanup(func() { cgroupPaths = saved })

	Configure()

	if got := debug.SetMemoryLimit(-1); got != before {
		t.Errorf("runtime limit changed to %d with no cgroup limit; want it left at %d", got, before)
	}
}

// A limit so small that targeting 80% of it would leave the collector thrashing
// is refused rather than honored.
func TestConfigureRefusesTinyLimits(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "")

	before := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(before) })

	saved := cgroupPaths
	cgroupPaths = write(t, "v2", strconv.Itoa(16<<20))
	t.Cleanup(func() { cgroupPaths = saved })

	Configure()

	if got := debug.SetMemoryLimit(-1); got != before {
		t.Errorf("runtime limit set to %d from a 16MiB container; want it left at %d", got, before)
	}
}
