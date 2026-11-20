package handler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// maxRetryAttempts caps retries. The failure hook sees the attempt number
// and normally decides when to stop; this only bounds a hook that always
// retries.
const maxRetryAttempts = 5

// ErrRetryLimit means an invocation has used all of its attempts.
var ErrRetryLimit = errors.New("retry limit reached")

// FailureNotifier receives every invocation that completed with a
// FailureError.
type FailureNotifier interface {
	Notify(invoke *pb.DispatchHandlerInvoke, failure *pb.InvocationFailure)
}

// FailureManager holds failed invocations until their client's failure hook
// says what to do with them: drop them, or retry them through the Manager.
type FailureManager interface {
	FailureNotifier
	// Dequeue hands out the next failure for a client. It is out of the queue
	// until the caller resolves or requeues it.
	InvocationSource[*FailureEvent]

	// Subscribe starts keeping failures for a dispatch id. Failures for a
	// dispatch id nobody subscribed are logged and discarded.
	Subscribe(dispatchId string)
}

// FailureEvent is a failure handed to a client. Exactly one of Resolve or
// Requeue takes effect; later calls do nothing.
type FailureEvent struct {
	*pb.DispatchEvent
	fm   *failureManager
	q    *failureQueue
	done atomic.Bool
}

// Resolve applies the client's answer.
func (e *FailureEvent) Resolve(action pb.FailureAction) error {
	if !e.done.CompareAndSwap(false, true) {
		return fmt.Errorf("failure for invocation %s already handled", e.Invoke.InvocationId)
	}
	return e.fm.resolve(e.DispatchEvent, action)
}

// Requeue puts the failure back to be handed out again, ahead of the rest.
func (e *FailureEvent) Requeue() {
	if e.done.CompareAndSwap(false, true) {
		e.q.push(e.DispatchEvent, true)
	}
}

type failureManager struct {
	logger  *zap.Logger
	manager Manager

	mu      sync.Mutex
	queues  map[string]*failureQueue
	pending prometheus.Gauge
}

// NewFailureManager creates a FailureManager and has manager report its
// failures to it.
func NewFailureManager(logger *zap.Logger, metrics *prometheus.Registry, manager Manager) FailureManager {
	fm := &failureManager{
		logger:  logger.Named("failure-manager"),
		manager: manager,
		queues:  make(map[string]*failureQueue),
		pending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "axon_handler_failure_events_pending",
			Help: "Number of failure events waiting for a client to resolve them",
		}),
	}
	if metrics != nil {
		metrics.MustRegister(fm.pending)
	}
	manager.SetFailureNotifier(fm)
	return fm
}

func (f *failureManager) queue(dispatchId string, create bool) *failureQueue {
	f.mu.Lock()
	defer f.mu.Unlock()
	q, ok := f.queues[dispatchId]
	if !ok && create {
		q = newFailureQueue()
		f.queues[dispatchId] = q
	}
	return q
}

func (f *failureManager) Subscribe(dispatchId string) {
	f.queue(dispatchId, true)
}

func (f *failureManager) Notify(invoke *pb.DispatchHandlerInvoke, failure *pb.InvocationFailure) {
	logger := f.logger.With(
		zap.String("handler", invoke.HandlerName),
		zap.String("invocation-id", invoke.InvocationId),
		zap.Int32("attempt", invoke.Attempt),
		zap.String("reason", failure.Reason.String()),
		zap.String("source", failure.Source.String()),
		zap.String("message", failure.Message),
	)
	q := f.queue(invoke.DispatchId, false)
	if q == nil {
		logger.Warn("Invocation failed, client does not handle failure events")
		return
	}
	logger.Warn("Invocation failed, queueing failure event")
	q.push(&pb.DispatchEvent{Invoke: invoke, Failure: failure}, false)
	f.pending.Inc()
}

func (f *failureManager) Dequeue(ctx context.Context, dispatchId string) (*FailureEvent, error) {
	q := f.queue(dispatchId, true)
	ev, err := q.pop(ctx)
	if err != nil {
		return nil, err
	}
	return &FailureEvent{DispatchEvent: ev, fm: f, q: q}, nil
}

func (f *failureManager) resolve(ev *pb.DispatchEvent, action pb.FailureAction) error {
	f.pending.Dec()
	logger := f.logger.With(
		zap.String("handler", ev.Invoke.HandlerName),
		zap.String("invocation-id", ev.Invoke.InvocationId),
		zap.String("action", action.String()),
	)
	if action != pb.FailureAction_FAILURE_ACTION_RETRY {
		logger.Debug("Dropping failed invocation")
		return nil
	}
	id, err := f.retry(ev.Invoke)
	if err != nil {
		logger.Warn("Not retrying failed invocation", zap.Error(err))
		return err
	}
	logger.Info("Retrying failed invocation", zap.String("retry-invocation-id", id))
	return nil
}

// retry queues invoke again as its next attempt. A retry that can't be
// queued fails like any other invocation, so it comes back as a failure.
func (f *failureManager) retry(invoke *pb.DispatchHandlerInvoke) (string, error) {
	attempt := max(invoke.Attempt, 1)
	if attempt >= maxRetryAttempts {
		return "", fmt.Errorf("%w: attempt %d of %d", ErrRetryLimit, attempt, maxRetryAttempts)
	}

	var entry HandlerEntry
	for _, e := range f.manager.ListHandlers() {
		if e.Id() == invoke.HandlerId {
			entry = e
			break
		}
		// The client re-registered since the invocation was sent, which
		// gives the handler a new id under the same dispatch id and name.
		if e.DispatchId() == invoke.DispatchId && e.Name() == invoke.HandlerName {
			entry = e
		}
	}
	if entry == nil {
		return "", fmt.Errorf("handler not found: %s", invoke.HandlerName)
	}

	next := NewHandlerInvoke(entry, invoke.Reason, invoke.Args)
	next.Attempt = attempt + 1
	if err := f.manager.Trigger(next); err != nil {
		return "", err
	}
	return next.Id, nil
}

// failureQueue holds one dispatch id's failures until they are handed to its
// client. It is unbounded on purpose: invocations are throttled, but a
// dropped failure is a message lost without a trace, which is what failure
// events exist to prevent.
type failureQueue struct {
	mu     sync.Mutex
	events []*pb.DispatchEvent
	// ready is closed while events is non-empty. Every waiter wakes, and the
	// first to take the lock gets the event; the rest wait again.
	ready chan struct{}
}

func newFailureQueue() *failureQueue {
	return &failureQueue{ready: make(chan struct{})}
}

func (q *failureQueue) push(ev *pb.DispatchEvent, front bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if front {
		q.events = append([]*pb.DispatchEvent{ev}, q.events...)
	} else {
		q.events = append(q.events, ev)
	}
	if len(q.events) == 1 {
		close(q.ready)
	}
}

// pop takes the oldest event, waiting until there is one or ctx ends.
func (q *failureQueue) pop(ctx context.Context) (*pb.DispatchEvent, error) {
	for {
		q.mu.Lock()
		if len(q.events) > 0 {
			ev := q.events[0]
			q.events[0] = nil
			q.events = q.events[1:]
			if len(q.events) == 0 {
				q.ready = make(chan struct{})
			}
			q.mu.Unlock()
			return ev, nil
		}
		ready := q.ready
		q.mu.Unlock()

		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
