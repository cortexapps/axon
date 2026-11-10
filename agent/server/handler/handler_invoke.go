package handler

import (
	"context"
	"fmt"
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

	// ClaimDispatch and ClaimExpired decide, exactly once, whether a queued
	// invocation was sent to a client or expired before one picked it up.
	ClaimDispatch() bool
	ClaimExpired() bool
}

const (
	invokeQueued int32 = iota
	invokeDispatched
	invokeExpired
)

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

func (h HandlerInvoke) GetEntry() HandlerEntry {
	return h.Entry
}
func (h HandlerInvoke) GetName() string {
	return h.Entry.Name()
}

func (h HandlerInvoke) GetReason() pb.HandlerInvokeType {
	return h.Reason
}

func (h *HandlerInvoke) Start(ctx context.Context) {
	if h.started.CompareAndSwap(false, true) {
		go func() {
			select {
			case <-ctx.Done():
				err := ctx.Err()
				if err == nil {
					err = fmt.Errorf("context cancelled")
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

func (h HandlerInvoke) GetResult() (string, error) {
	if h.finished.Load() {
		return h.result, h.err
	}
	return "", fmt.Errorf("handler not finished")
}

func (h HandlerInvoke) Done() <-chan struct{} {
	return h.done
}

func (h HandlerInvoke) ClaimDispatch() bool {
	return h.state.CompareAndSwap(invokeQueued, invokeDispatched)
}

func (h HandlerInvoke) ClaimExpired() bool {
	return h.state.CompareAndSwap(invokeQueued, invokeExpired)
}

func (h HandlerInvoke) ToDispatchInvoke() *pb.DispatchHandlerInvoke {
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
