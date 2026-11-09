package grpctunnel

import (
	"context"
	"errors"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon/tunnelpb"
	"go.uber.org/zap"
)

// callProgress records where a call is, so that whatever stops it can say
// where it was and for how long. Only the call's own goroutine moves it on,
// and only that goroutine reads it.
type callProgress struct {
	receivedAt   time.Time
	stage        pb.CallCancel_Stage
	stageAt      time.Time
	upstreamHost string
}

func newCallProgress(now time.Time) callProgress {
	return callProgress{receivedAt: now, stage: pb.CallCancel_QUEUED_FOR_CAPACITY, stageAt: now}
}

func (p *callProgress) enter(stage pb.CallCancel_Stage) {
	p.stage = stage
	p.stageAt = time.Now()
}

// report builds the CallCancel that tells the server where the call stopped
// and why.
func (p *callProgress) report(code int32, reason string, cause pb.CallCancel_Cause) *pb.CallCancel {
	now := time.Now()
	return &pb.CallCancel{
		Code:           code,
		Reason:         reason,
		Stage:          p.stage,
		Cause:          cause,
		StageElapsedMs: now.Sub(p.stageAt).Milliseconds(),
		TotalElapsedMs: now.Sub(p.receivedAt).Milliseconds(),
		UpstreamHost:   p.upstreamHost,
	}
}

// fields renders the same account for the agent's own log line.
func (p *callProgress) fields(cause pb.CallCancel_Cause) []zap.Field {
	now := time.Now()
	return []zap.Field{
		zap.Stringer("stage", p.stage),
		zap.Stringer("cause", cause),
		zap.Int64("stageElapsedMs", now.Sub(p.stageAt).Milliseconds()),
		zap.Int64("totalElapsedMs", now.Sub(p.receivedAt).Milliseconds()),
	}
}

// stopCause says why a call's context ended, or why its upstream failed.
func (tc *tunnelClient) stopCause(ctx context.Context, c *agentCall) pb.CallCancel_Cause {
	switch {
	case c.serverCancelled.Load():
		return pb.CallCancel_CANCELED_BY_SERVER
	case tc.parentCtx != nil && tc.parentCtx.Err() != nil:
		return pb.CallCancel_AGENT_SHUTDOWN
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return pb.CallCancel_AGENT_TIMEOUT
	}
	return pb.CallCancel_UPSTREAM_ERROR
}
