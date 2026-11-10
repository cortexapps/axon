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

// InvocationInfo identifies one invocation for the dispatch hooks. For a
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

// PostAction is what a PostDispatchHook wants done with a failed invocation.
type PostAction int

const (
	// PostActionIgnore leaves the failure as it is.
	PostActionIgnore PostAction = iota
	// PostActionRetry queues the invocation again as its next attempt.
	PostActionRetry
)

// PreDispatchHook runs before each invocation's handler.
type PreDispatchHook func(ctx context.Context, info InvocationInfo)

// PostDispatchHook runs after each invocation, with a nil failure when it
// succeeded. It also runs for failures the agent detects on its own, such as
// an invocation that never reached this process, so it sees every failed
// message. Its return value only matters for a failure.
type PostDispatchHook func(ctx context.Context, info InvocationInfo, failure *Failure) PostAction

// WithPreDispatchHook sets a hook that runs before every handler.
func WithPreDispatchHook(hook PreDispatchHook) Option {
	return func(a *agentOptions) {
		a.preDispatch = hook
	}
}

// WithPostDispatchHook sets a hook that runs after every invocation and for
// every failure, including ones the agent detects.
func WithPostDispatchHook(hook PostDispatchHook) Option {
	return func(a *agentOptions) {
		a.postDispatch = hook
	}
}

func (a *Agent) runPreDispatch(info InvocationInfo) {
	if a.preDispatch == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			a.logger.Error("panic in pre-dispatch hook", zap.Any("panic", r))
		}
	}()
	a.preDispatch(context.Background(), info)
}

func (a *Agent) runPostDispatch(info InvocationInfo, failure *Failure) {
	if a.postDispatch == nil {
		return
	}
	if a.callPostDispatch(info, failure) == PostActionRetry && failure != nil {
		a.retryInvocation(info)
	}
}

func (a *Agent) callPostDispatch(info InvocationInfo, failure *Failure) (action PostAction) {
	defer func() {
		if r := recover(); r != nil {
			a.logger.Error("panic in post-dispatch hook", zap.Any("panic", r))
			action = PostActionIgnore
		}
	}()
	return a.postDispatch(context.Background(), info, failure)
}

func (a *Agent) retryInvocation(info InvocationInfo) {
	stub := a.client.agent()
	if stub == nil {
		a.logger.Error("cannot retry invocation, no agent connection",
			zap.String("invocation-id", info.InvocationID))
		return
	}
	resp, err := stub.RetryInvocation(context.Background(), &pb.RetryInvocationRequest{Invoke: info.invoke})
	if status.Code(err) == codes.Unimplemented {
		a.logger.Warn("agent does not support retries, upgrade the agent",
			zap.String("invocation-id", info.InvocationID))
		return
	}
	if err != nil {
		a.logger.Error("failed to retry invocation",
			zap.String("invocation-id", info.InvocationID), zap.Error(err))
		return
	}
	if resp.GetError() != nil {
		a.logger.Warn("agent rejected retry",
			zap.String("invocation-id", info.InvocationID),
			zap.String("error", resp.GetError().GetMessage()))
		return
	}
	a.logger.Info("retrying invocation",
		zap.String("invocation-id", info.InvocationID),
		zap.String("retry-invocation-id", resp.GetInvocationId()),
		zap.Int("attempt", info.Attempt+1))
}

var errEventsUnsupported = errors.New("agent does not support failure events")

// runFailureEvents reads the agent's failure events until ctx is done. It has
// its own stream so it keeps reading while every handler is busy.
func (a *Agent) runFailureEvents(ctx context.Context) {
	for ctx.Err() == nil {
		err := a.readFailureEvents(ctx)
		if errors.Is(err, errEventsUnsupported) {
			a.logger.Warn("agent does not send failure events, upgrade the agent to receive them")
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

func (a *Agent) readFailureEvents(ctx context.Context) error {
	stub := a.client.agent()
	if stub == nil {
		return fmt.Errorf("failed to create agent connection")
	}
	stream, err := stub.DispatchEvents(ctx, &pb.DispatchRequest{
		DispatchId:    a.DispatchId,
		ClientVersion: version.Client,
		FailureEvents: true,
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
		a.runPostDispatch(newInvocationInfo(ev.GetInvoke()), failureFromProto(ev.GetFailure()))
	}
}
