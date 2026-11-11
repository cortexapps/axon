package handler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

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
	// Next hands out the next failure for a client. It stays pending until
	// Resolve or Requeue.
	InvocationSource[*pb.DispatchEvent]

	// Subscribe starts keeping failures for a dispatch id. Failures for a
	// dispatch id nobody subscribed are logged and discarded.
	Subscribe(dispatchId string)
	// Resolve applies the client's answer to a pending failure.
	Resolve(dispatchId string, resolution *pb.FailureResolution) error
	// Requeue puts a pending failure back to be sent again, first.
	Requeue(dispatchId string, invocationId string)
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
	q.push(&pb.DispatchEvent{Invoke: invoke, Failure: failure})
	f.pending.Inc()
}

func (f *failureManager) Next(ctx context.Context, dispatchId string, wait time.Duration) (*pb.DispatchEvent, error) {
	return f.queue(dispatchId, true).next(ctx, wait), nil
}

func (f *failureManager) Requeue(dispatchId string, invocationId string) {
	f.queue(dispatchId, true).requeue(invocationId)
}

func (f *failureManager) Resolve(dispatchId string, resolution *pb.FailureResolution) error {
	ev := f.queue(dispatchId, true).take(resolution.GetInvocationId())
	if ev == nil {
		return fmt.Errorf("no pending failure for invocation %s", resolution.GetInvocationId())
	}
	f.pending.Dec()

	logger := f.logger.With(
		zap.String("handler", ev.Invoke.HandlerName),
		zap.String("invocation-id", ev.Invoke.InvocationId),
		zap.String("action", resolution.GetAction().String()),
	)
	if resolution.GetAction() != pb.FailureAction_FAILURE_ACTION_RETRY {
		logger.Info("Dropping failed invocation")
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

// failureQueue holds one dispatch id's failures until its client resolves
// them. It is unbounded on purpose: invocations are throttled, but a dropped
// failure is a message lost without a trace, which is what failure events
// exist to prevent.
type failureQueue struct {
	mu      sync.Mutex
	events  []*pb.DispatchEvent
	pending map[string]*pb.DispatchEvent
	// notify has room for one wakeup, so push never blocks and a waiting
	// next never misses an event that arrives between checks.
	notify chan struct{}
}

func newFailureQueue() *failureQueue {
	return &failureQueue{
		pending: make(map[string]*pb.DispatchEvent),
		notify:  make(chan struct{}, 1),
	}
}

func (q *failureQueue) push(ev *pb.DispatchEvent) {
	q.mu.Lock()
	q.events = append(q.events, ev)
	q.mu.Unlock()
	q.wake()
}

func (q *failureQueue) wake() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// next moves the oldest event to pending and returns it, waiting up to wait
// for one. It returns nil if none arrived.
func (q *failureQueue) next(ctx context.Context, wait time.Duration) *pb.DispatchEvent {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		q.mu.Lock()
		if len(q.events) > 0 {
			ev := q.events[0]
			q.events[0] = nil
			q.events = q.events[1:]
			q.pending[ev.Invoke.InvocationId] = ev
			remaining := len(q.events)
			q.mu.Unlock()
			if remaining > 0 {
				// Another stream may be waiting on the same queue.
				q.wake()
			}
			return ev
		}
		q.mu.Unlock()

		select {
		case <-q.notify:
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

func (q *failureQueue) take(invocationId string) *pb.DispatchEvent {
	q.mu.Lock()
	defer q.mu.Unlock()
	ev := q.pending[invocationId]
	delete(q.pending, invocationId)
	return ev
}

func (q *failureQueue) requeue(invocationId string) {
	q.mu.Lock()
	ev, ok := q.pending[invocationId]
	if ok {
		delete(q.pending, invocationId)
		q.events = append([]*pb.DispatchEvent{ev}, q.events...)
	}
	q.mu.Unlock()
	if ok {
		q.wake()
	}
}
