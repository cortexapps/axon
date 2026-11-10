package config

import (
	"math"
	"strconv"
	"testing"
)

func TestMaxFrameBytesFromEnv(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		want      int
		wantPanic bool
	}{
		{"unset uses the default", "", DefaultMaxFrameBytes, false},
		{"a sane override is honored", "2097152", 2 * 1024 * 1024, false},
		{"the ceiling itself is allowed", strconv.Itoa(MaxAllowedFrameBytes), MaxAllowedFrameBytes, false},
		{"above the ceiling is rejected", strconv.Itoa(MaxAllowedFrameBytes + 1), 0, true},
		{"a value that would wrap int32 is rejected", strconv.Itoa(math.MaxInt32), 0, true},
		{"zero is rejected", "0", 0, true},
		{"negative is rejected", "-1", 0, true},
		{"garbage is rejected", "banana", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv("MAX_FRAME_BYTES", tt.env)
			}

			defer func() {
				r := recover()
				if tt.wantPanic && r == nil {
					t.Errorf("MAX_FRAME_BYTES=%q was accepted; want rejection", tt.env)
				}
				if !tt.wantPanic && r != nil {
					t.Errorf("MAX_FRAME_BYTES=%q panicked: %v", tt.env, r)
				}
			}()

			cfg := NewConfigFromEnv()
			if !tt.wantPanic && cfg.MaxFrameBytes != tt.want {
				t.Errorf("MaxFrameBytes = %d, want %d", cfg.MaxFrameBytes, tt.want)
			}
		})
	}
}

// ServerHello carries MaxFrameBytes as an int32, so the largest value the
// config will accept has to fit in one -- otherwise it reaches agents wrapped
// and negative, and they silently fall back to their own default.
func TestAcceptedFrameBytesFitInInt32(t *testing.T) {
	if MaxAllowedFrameBytes > math.MaxInt32 {
		t.Fatalf("MaxAllowedFrameBytes = %d exceeds math.MaxInt32 (%d)", MaxAllowedFrameBytes, math.MaxInt32)
	}
	if MaxAllowedFrameBytes <= 0 {
		t.Fatalf("MaxAllowedFrameBytes = %d must be positive", MaxAllowedFrameBytes)
	}
	if DefaultMaxFrameBytes > MaxAllowedFrameBytes {
		t.Fatalf("DefaultMaxFrameBytes = %d exceeds the accepted maximum %d", DefaultMaxFrameBytes, MaxAllowedFrameBytes)
	}
}
