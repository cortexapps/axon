package axon

import (
	"context"
	"errors"
	"fmt"
	"time"

	pb "github.com/cortexapps/axon-go/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon-go/version"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// InvocationInfo identifies one invocation for the failure hook. For a
// webhook, Args holds the inbound message: "body", "content-type" and "url".
type InvocationInfo struct {
	InvocationID string
	HandlerName  string
	InvokeType   pb.HandlerInvokeType
	Args         map[string]string
	// Attempt is 1 for the first delivery and counts up with each retry.
	Attempt int

	invoke *pb.DispatchHandlerInvoke
}

func newInvocationInfo(invoke *pb.DispatchHandlerInvoke) InvocationInfo {
	return InvocationInfo{
		InvocationID: invoke.InvocationId,
		HandlerName:  invoke.HandlerName,
		InvokeType:   invoke.Reason,
		Args:         invoke.Args,
		Attempt:      max(int(invoke.Attempt), 1),
		invoke:       invoke,
	}
}

// Failure describes why an invocation failed. Source says whether handler
// code failed (APPLICATION) or Axon did (RUNTIME).
type Failure struct {
	Reason  pb.FailureReason
	Source  pb.FailureSource
	Message string
	// CallerRejected means whoever sent the invocation was told it was not
	// accepted (a webhook sender got a 503), so it may arrive again.
	CallerRejected bool
}

func failureFromProto(f *pb.InvocationFailure) *Failure {
	return &Failure{
		Reason:         f.GetReason(),
		Source:         f.GetSource(),
		Message:        f.GetMessage(),
		CallerRejected: f.GetCallerRejected(),
	}
}

func (f *Failure) toProto() *pb.InvocationFailure {
	return &pb.InvocationFailure{
		Reason:         f.Reason,
		Source:         f.Source,
		Message:        f.Message,
		CallerRejected: f.CallerRejected,
	}
}

// FailureAction is what a FailureHook wants the agent to do with a failed
// invocation.
type FailureAction int

const (
	// FailureActionDrop does nothing more with the invocation.
	FailureActionDrop FailureAction = iota
	// FailureActionRetry has the agent queue the invocation again as its
	// next attempt.
	FailureActionRetry
)

func (a FailureAction) toProto() pb.FailureAction {
	if a == FailureActionRetry {
		return pb.FailureAction_FAILURE_ACTION_RETRY
	}
	return pb.FailureAction_FAILURE_ACTION_DROP
}

// FailureHook runs for every failed invocation, wherever it failed: in this
// process, or in the agent, such as an invocation that never reached this
// process. The agent sends each failure here and applies the answer.
type FailureHook func(ctx context.Context, info InvocationInfo, failure *Failure) FailureAction

// WithFailureHook sets a hook that runs for every failed invocation.
func WithFailureHook(hook FailureHook) Option {
	return func(a *agentOptions) {
		a.onFailure = hook
	}
}

func (a *Agent) runFailureHook(info InvocationInfo, failure *Failure) (action FailureAction) {
	defer func() {
		if r := recover(); r != nil {
			a.logger.Error("panic in failure hook, dropping the invocation",
				zap.String("invocation-id", info.InvocationID), zap.Any("panic", r))
			action = FailureActionDrop
		}
	}()
	return a.onFailure(context.Background(), info, failure)
}

var errEventsUnsupported = errors.New("agent does not support failure events")

// runFailureEvents reads the agent's failure events until ctx is done. It has
// its own stream so it keeps reading while every handler is busy.
func (a *Agent) runFailureEvents(ctx context.Context) {
	for ctx.Err() == nil {
		err := a.readFailureEvents(ctx)
		if errors.Is(err, errEventsUnsupported) {
			a.logger.Warn("agent does not send failure events, upgrade the agent to use a failure hook")
			return
		}
		if ctx.Err() != nil {
			return
		}
		a.logger.Warn("failure event stream ended, reconnecting in "+sleepErrorWait.String(), zap.Error(err))
		select {
		case <-ctx.Done():
		case <-time.After(sleepErrorWait):
		}
	}
}

// readFailureEvents runs the hook for each failure the agent sends and
// answers with what it returned.
func (a *Agent) readFailureEvents(ctx context.Context) error {
	stub := a.client.agent()
	if stub == nil {
		return fmt.Errorf("failed to create agent connection")
	}
	stream, err := stub.DispatchEvents(ctx)
	if err != nil {
		return err
	}
	err = stream.Send(&pb.DispatchEventsRequest{
		DispatchId:    a.DispatchId,
		ClientVersion: version.Client,
	})
	if err != nil {
		return err
	}
	for {
		ev, err := stream.Recv()
		if status.Code(err) == codes.Unimplemented {
			return errEventsUnsupported
		}
		if err != nil {
			return err
		}
		if ev.GetInvoke() == nil {
			continue
		}
		action := a.runFailureHook(newInvocationInfo(ev.GetInvoke()), failureFromProto(ev.GetFailure()))
		err = stream.Send(&pb.DispatchEventsRequest{
			Resolution: &pb.FailureResolution{
				InvocationId: ev.GetInvoke().GetInvocationId(),
				Action:       action.toProto(),
			},
		})
		if err != nil {
			return err
		}
	}
}
