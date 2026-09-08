package grpctunnel

import (
	"testing"

	"go.uber.org/zap"
)

func TestClampFrameBytes(t *testing.T) {
	tests := []struct {
		name      string
		announced int32
		want      int32
	}{
		{"typical 1MiB default is untouched", 1024 * 1024, 1024 * 1024},
		{"at the ceiling is untouched", maxAcceptedFrameBytes, maxAcceptedFrameBytes},
		{"above the ceiling is clamped", 64 * 1024 * 1024, maxAcceptedFrameBytes},
		{"zero passes through so the caller can default", 0, 0},
		{"negative passes through so the caller can default", -1, -1},
		{"small frames are honored", 16 * 1024, 16 * 1024},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampFrameBytes(tt.announced, zap.NewNop()); got != tt.want {
				t.Errorf("clampFrameBytes(%d) = %d, want %d", tt.announced, got, tt.want)
			}
		})
	}
}

// A server that announces int32's maximum would otherwise have the agent try to
// allocate 2GiB per in-flight call.
func TestClampFrameBytesBoundsWorstCase(t *testing.T) {
	const maxInflight = 256
	got := clampFrameBytes(1<<31-1, zap.NewNop())

	ceiling := int64(got) * maxInflight
	if ceiling > 1<<30 {
		t.Errorf("worst-case ceiling %d bytes exceeds 1GiB across %d in-flight calls", ceiling, maxInflight)
	}
}

// The clamp must tolerate a nil logger: it runs on the handshake path, and a
// panic there would take down the stream rather than degrade it.
func TestClampFrameBytesNilLogger(t *testing.T) {
	if got := clampFrameBytes(64*1024*1024, nil); got != maxAcceptedFrameBytes {
		t.Errorf("got %d, want %d", got, maxAcceptedFrameBytes)
	}
}
