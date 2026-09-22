package grpctunnel

import "go.uber.org/zap"

// clampFrameBytes bounds the frame size announced in the server's hello.
//
// A non-positive value means the server expressed no preference, and callers
// fall back to maxChunkSize. Anything above maxAcceptedFrameBytes is treated as
// a server-side misconfiguration rather than an instruction: honoring it would
// scale the agent's per-call buffer, and so its whole memory ceiling, by a
// number set somewhere the agent's operator cannot see or override.
func clampFrameBytes(announced int32, logger *zap.Logger) int32 {
	if announced <= 0 {
		return announced // caller substitutes maxChunkSize
	}
	if announced > maxAcceptedFrameBytes {
		if logger != nil {
			logger.Warn("Server announced a frame size above the agent's accepted maximum; clamping",
				zap.Int32("announcedBytes", announced),
				zap.Int("clampedToBytes", maxAcceptedFrameBytes),
			)
		}
		return maxAcceptedFrameBytes
	}
	return announced
}
