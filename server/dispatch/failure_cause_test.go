package dispatch

import (
	"context"
	"fmt"
	"testing"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
	"github.com/stretchr/testify/assert"
)

func TestCancelError_FailureCause(t *testing.T) {
	for _, tc := range []struct {
		stage pb.CallCancel_Stage
		cause pb.CallCancel_Cause
		want  FailureCause
	}{
		{pb.CallCancel_AWAITING_UPSTREAM_HEADERS, pb.CallCancel_CANCELED_BY_SERVER, CauseUpstreamSlow},
		{pb.CallCancel_AWAITING_UPSTREAM_HEADERS, pb.CallCancel_AGENT_TIMEOUT, CauseUpstreamSlow},
		{pb.CallCancel_AWAITING_UPSTREAM_HEADERS, pb.CallCancel_UPSTREAM_ERROR, CauseUpstreamError},
		{pb.CallCancel_STREAMING_BODY, pb.CallCancel_UPSTREAM_ERROR, CauseUpstreamError},
		{pb.CallCancel_QUEUED_FOR_CAPACITY, pb.CallCancel_CANCELED_BY_SERVER, CauseAgentAtCapacity},
		{pb.CallCancel_QUEUED_FOR_CAPACITY, pb.CallCancel_CAPACITY, CauseAgentAtCapacity},
		{pb.CallCancel_ROUTING, pb.CallCancel_ROUTE_REJECTED, CauseRouteRejected},
		{pb.CallCancel_AWAITING_UPSTREAM_HEADERS, pb.CallCancel_AGENT_SHUTDOWN, CauseAgentShutdown},
		// An agent that predates stage and cause reporting.
		{pb.CallCancel_STAGE_UNSPECIFIED, pb.CallCancel_CAUSE_UNSPECIFIED, CauseAgentCancel},
	} {
		t.Run(fmt.Sprintf("%s/%s", tc.stage, tc.cause), func(t *testing.T) {
			e := &CancelError{Stage: tc.stage, Cause: tc.cause}
			assert.Equal(t, tc.want, e.FailureCause())
		})
	}
}

func TestDispatchError_Cause(t *testing.T) {
	slowUpstream := &CancelError{Stage: pb.CallCancel_AWAITING_UPSTREAM_HEADERS, Cause: pb.CallCancel_CANCELED_BY_SERVER}
	sent := func(kind FailureKind) *DispatchError {
		return &DispatchError{Kind: kind, Phase: PhaseAwaitingResponse, CallID: "c", Err: context.DeadlineExceeded}
	}
	for _, tc := range []struct {
		name    string
		err     *DispatchError
		report  *CancelError
		outcome AgentOutcome
		want    FailureCause
	}{
		{"no tunnel", &DispatchError{Kind: FailureNoTunnel, Phase: PhaseAcquire, Err: ErrNoTunnel}, nil, AgentNotAsked, CauseNoTunnel},
		{"never got a stream", &DispatchError{Kind: FailureCallerCanceled, Phase: PhaseAcquire, Err: context.Canceled}, nil, AgentNotAsked, CauseStreamsExhausted},
		{"sent, agent says upstream", sent(FailureResponseTimeout), slowUpstream, AgentReported, CauseUpstreamSlow},
		{"sent, agent finished late", sent(FailureCallerCanceled), nil, AgentCompletedLate, CauseUpstreamSlow},
		{"sent, agent silent", sent(FailureResponseTimeout), nil, AgentSilent, CauseAgentUnresponsive},
		{"sent, not asked yet", sent(FailureResponseTimeout), nil, AgentNotAsked, CauseAwaitingAgent},
		{"stream closed", sent(FailureStreamClosed), nil, AgentNotAsked, CauseTunnelError},
		{"request body", sent(FailureRequestBody), nil, AgentNotAsked, CauseCallerBodyError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.err.Cause(tc.report, tc.outcome))
		})
	}
}
