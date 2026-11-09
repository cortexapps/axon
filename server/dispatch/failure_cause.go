package dispatch

import (
	"errors"

	pb "github.com/cortexapps/axon-server/.generated/proto/tunnelpb"
)

// FailureCause names what went wrong with a dispatch in terms of what to fix,
// as opposed to its FailureKind, which names who gave up. A caller that times
// out after 60s is the same Kind whether every stream was held by slow calls
// or its own call sat waiting on the upstream; the cause tells those apart.
// It is logged, counted, and returned to the caller in a response header.
type FailureCause string

const (
	// CauseNoTunnel: this server holds no stream for the token.
	CauseNoTunnel FailureCause = "no_tunnel"
	// CauseStreamsExhausted: every stream for the token on this server stayed
	// busy until the deadline. The calls holding them are the problem, not
	// this one; the failure's StreamState names them.
	CauseStreamsExhausted FailureCause = "streams_exhausted"
	// CauseUpstreamSlow: the agent sent the request upstream and no response
	// headers came back before someone gave up -- or they came back after.
	CauseUpstreamSlow FailureCause = "upstream_slow"
	// CauseUpstreamError: the upstream failed outright (connection error,
	// reset, unreadable body).
	CauseUpstreamError FailureCause = "upstream_error"
	// CauseAgentAtCapacity: the call queued for the agent's own in-flight
	// capacity and never got it.
	CauseAgentAtCapacity FailureCause = "agent_at_capacity"
	// CauseRouteRejected: no route matched the request, or the route refused it.
	CauseRouteRejected FailureCause = "route_rejected"
	// CauseAgentShutdown: the agent was shutting down or restarting.
	CauseAgentShutdown FailureCause = "agent_shutdown"
	// CauseAgentUnresponsive: the server cancelled the call and the agent did
	// not answer within the grace period.
	CauseAgentUnresponsive FailureCause = "agent_unresponsive"
	// CauseAgentCancel: the agent cancelled without saying why (an agent that
	// predates stage and cause reporting).
	CauseAgentCancel FailureCause = "agent_cancel"
	// CauseAwaitingAgent: the call was with the agent when the server gave up,
	// and the agent's account is not in (yet, or at all when the grace period
	// is off).
	CauseAwaitingAgent FailureCause = "awaiting_agent"
	// CauseTunnelError: the stream failed under the call (send failure, closed).
	CauseTunnelError FailureCause = "tunnel_error"
	// CauseCallerBodyError: reading the caller's request body failed.
	CauseCallerBodyError FailureCause = "caller_body_error"
	// CauseUnknown: not a dispatch failure the server can classify.
	CauseUnknown FailureCause = "unknown"
)

// MadeCallerWait reports whether the cause is something on our side keeping
// the caller waiting, as opposed to a caller simply leaving. A caller giving
// up on one of these is the symptom, not the routine event a caller
// disconnect otherwise is.
func (c FailureCause) MadeCallerWait() bool {
	switch c {
	case CauseStreamsExhausted, CauseAgentUnresponsive, CauseAgentAtCapacity, CauseUpstreamSlow:
		return true
	}
	return false
}

// FailureCause reads the agent's account of where a call was and why it
// stopped. An agent too old to report a stage or cause gives CauseAgentCancel.
func (e *CancelError) FailureCause() FailureCause {
	switch {
	case e.Cause == pb.CallCancel_CAPACITY || e.Stage == pb.CallCancel_QUEUED_FOR_CAPACITY:
		return CauseAgentAtCapacity
	case e.Cause == pb.CallCancel_ROUTE_REJECTED:
		return CauseRouteRejected
	case e.Cause == pb.CallCancel_AGENT_SHUTDOWN:
		return CauseAgentShutdown
	case e.Cause == pb.CallCancel_UPSTREAM_ERROR:
		return CauseUpstreamError
	case e.Stage == pb.CallCancel_AWAITING_UPSTREAM_HEADERS:
		return CauseUpstreamSlow
	}
	return CauseAgentCancel
}

// Cause classifies the failure. report and outcome are the agent's account,
// from AwaitAgentReport; before it is in, pass (nil, AgentNotAsked) and the
// cause is what the server alone can tell.
func (e *DispatchError) Cause(report *CancelError, outcome AgentOutcome) FailureCause {
	switch {
	case errors.Is(e.Err, ErrNoTunnel):
		return CauseNoTunnel
	case e.Phase == PhaseAcquire:
		return CauseStreamsExhausted
	}
	switch e.Kind {
	case FailureStreamClosed, FailureSendFailed:
		return CauseTunnelError
	case FailureRequestBody:
		return CauseCallerBodyError
	}
	if report != nil {
		return report.FailureCause()
	}
	switch outcome {
	case AgentCompletedLate:
		return CauseUpstreamSlow
	case AgentSilent:
		return CauseAgentUnresponsive
	}
	return CauseAwaitingAgent
}
