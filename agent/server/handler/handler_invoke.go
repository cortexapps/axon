package handler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/google/uuid"
)

type Invocable interface {
	GetEntry() HandlerEntry
	GetReason() pb.HandlerInvokeType
	ToDispatchInvoke() *pb.DispatchHandlerInvoke

	Start(ctx context.Context)
	Complete(result string, err error) error
	Done() <-chan struct{}
	GetResult() (string, error)

	// SetState moves the invocation to a new state, and reports false when
	// that transition isn't allowed from where it is now.
	SetState(state InvokeState) bool
	State() InvokeState
}

type InvokeState int32

const (
	// InvokeQueued is waiting for a client to take it.
	InvokeQueued InvokeState = iota
	// InvokeDispatched has been sent to a client.
	InvokeDispatched
	// InvokeExpired ran out of time before a client took it.
	InvokeExpired
	// InvokeRejected was refused because the queue was full.
	InvokeRejected
)

// invokeTransitions lists the states each state may move to. A queued
// invocation leaves the queue exactly once, one way or another.
var invokeTransitions = map[InvokeState][]InvokeState{
	InvokeQueued: {InvokeDispatched, InvokeExpired, InvokeRejected},
}

// FailureError completes an invocation that failed in a way its handler's
// failure hook should hear about. Every such failure, wherever it is
// detected, reaches the failure manager this one way.
type FailureError struct {
	Failure *pb.InvocationFailure
}

func (e *FailureError) Error() string {
	return fmt.Sprintf("%s (%s): %s", e.Failure.GetReason(), e.Failure.GetSource(), e.Failure.GetMessage())
}

func NewFailureError(reason pb.FailureReason, source pb.FailureSource, message string) *FailureError {
	return &FailureError{Failure: &pb.InvocationFailure{Reason: reason, Source: source, Message: message}}
}

type HandlerInvoke struct {
	Id        string
	Entry     HandlerEntry
	Reason    pb.HandlerInvokeType
	Args      map[string]string
	Timeout   time.Duration
	Timestamp time.Time
	Attempt   int32

	result string
	err    error
	done   chan struct{}

	started  *atomic.Bool
	finished *atomic.Bool
	state    *atomic.Int32
}

func NewHandlerInvoke(
	entry HandlerEntry,
	reason pb.HandlerInvokeType,
	args map[string]string,
) *HandlerInvoke {
	return &HandlerInvoke{
		Id:        uuid.New().String(),
		Entry:     entry,
		Reason:    reason,
		Args:      args,
		Timeout:   entry.Timeout(),
		Timestamp: time.Now(),
		Attempt:   1,

		done:     make(chan struct{}),
		started:  &atomic.Bool{},
		finished: &atomic.Bool{},
		state:    &atomic.Int32{},
	}
}

func (h *HandlerInvoke) GetEntry() HandlerEntry {
	return h.Entry
}
func (h *HandlerInvoke) GetName() string {
	return h.Entry.Name()
}

func (h *HandlerInvoke) GetReason() pb.HandlerInvokeType {
	return h.Reason
}

// Start bounds how long the invocation may wait in the queue. Once a client
// has it, its result comes from the client's report or the agent's own
// timeout instead.
func (h *HandlerInvoke) Start(ctx context.Context) {
	if h.started.CompareAndSwap(false, true) {
		go func() {
			select {
			case <-ctx.Done():
				if !h.SetState(InvokeExpired) {
					return
				}
				var err error = ctx.Err()
				if errors.Is(err, context.DeadlineExceeded) {
					err = NewFailureError(pb.FailureReason_NO_RESOURCES, pb.FailureSource_RUNTIME,
						fmt.Sprintf("expired after %v waiting for a free handler",
							time.Since(h.Timestamp).Round(time.Millisecond)))
				}
				h.Complete("", err)
			case <-h.done:
			}
		}()
		return
	}
}

func (h *HandlerInvoke) Complete(result string, err error) error {
	if h.finished.CompareAndSwap(false, true) {
		h.err = err
		if err == nil {
			h.result = result
		}
		close(h.done)
		return nil
	}
	return fmt.Errorf("handler already finished")
}

func (h *HandlerInvoke) GetResult() (string, error) {
	if h.finished.Load() {
		return h.result, h.err
	}
	return "", fmt.Errorf("handler not finished")
}

func (h *HandlerInvoke) Done() <-chan struct{} {
	return h.done
}

func (h *HandlerInvoke) SetState(to InvokeState) bool {
	for {
		from := InvokeState(h.state.Load())
		if !slices.Contains(invokeTransitions[from], to) {
			return false
		}
		if h.state.CompareAndSwap(int32(from), int32(to)) {
			return true
		}
	}
}

func (h *HandlerInvoke) State() InvokeState {
	return InvokeState(h.state.Load())
}

func (h *HandlerInvoke) ToDispatchInvoke() *pb.DispatchHandlerInvoke {
	return &pb.DispatchHandlerInvoke{
		InvocationId: h.Id,
		DispatchId:   h.Entry.DispatchId(),
		HandlerId:    h.Entry.Id(),
		HandlerName:  h.Entry.Name(),
		Reason:       h.Reason,
		Args:         h.Args,
		TimeoutMs:    int32(h.Entry.Timeout().Milliseconds()),
		Attempt:      h.Attempt,
	}
}
