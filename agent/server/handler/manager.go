package handler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/server/cron"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// dispatchQueueDepth is how many invocations may wait for an SDK client
// before Trigger starts refusing them.
const dispatchQueueDepth = 100

// ErrDispatchQueueFull means no SDK client drained the queue fast enough.
// Callers that front an inbound request must turn it into a retryable
// response rather than accepting work that will only time out.
var ErrDispatchQueueFull = errors.New("dispatch queue is full")

// maxRetryAttempts caps RetryInvocation. The failure hook sees the attempt
// number and normally decides when to stop; this only bounds a hook that
// always retries.
const maxRetryAttempts = 5

// ErrRetryLimit means an invocation has used all of its attempts.
var ErrRetryLimit = errors.New("retry limit reached")

type Manager interface {
	RegisterHandler(dispatchId string, name string, timeout time.Duration, options ...*pb.HandlerOption) (string, error)
	UnregisterHandler(id string)
	ListHandlers() []HandlerEntry
	ClearHandlers(id string)
	Start(id string) error
	Stop(id string) error
	Trigger(handler Invocable) error
	GetByTag(tag string) HandlerEntry
	Dequeue(ctx context.Context, id string, waitTime time.Duration) (Invocable, error)
	Close() error
	IsFinished() bool

	// SubscribeFailures starts keeping failure events for a dispatch id.
	// Events for a dispatch id nobody subscribed are logged and discarded.
	SubscribeFailures(dispatchId string)
	EmitFailure(invoke *pb.DispatchHandlerInvoke, failure *pb.InvocationFailure)
	// NextFailure blocks until a failure event is available or ctx is done.
	NextFailure(ctx context.Context, dispatchId string) (*pb.DispatchEvent, error)
	// RequeueFailure puts back an event that could not be delivered.
	RequeueFailure(dispatchId string, ev *pb.DispatchEvent)
	// Retrigger queues a failed invocation again as its next attempt and
	// returns the new invocation id.
	Retrigger(invoke *pb.DispatchHandlerInvoke) (string, error)
}

type handlerManager struct {
	logger              *zap.Logger
	dispatchQueues      map[string]chan Invocable
	outstandingRequests map[string]Invocable
	handlers            map[string]HandlerEntry
	cron                cron.Cron
	done                chan struct{}
	finished            bool
	invokeCounter       *prometheus.CounterVec
	queueDepthGauge     *prometheus.GaugeVec
	handlerLatencyGauge *prometheus.HistogramVec
	failuresPending     prometheus.Gauge

	failureLock   sync.Mutex
	failureQueues map[string]*failureQueue
}

func NewHandlerManager(logger *zap.Logger, cron cron.Cron, metrics *prometheus.Registry) Manager {

	mgr := &handlerManager{
		logger:              logger.Named("handler-manager"),
		handlers:            make(map[string]HandlerEntry),
		dispatchQueues:      make(map[string]chan Invocable),
		cron:                cron,
		outstandingRequests: make(map[string]Invocable),
		failureQueues:       make(map[string]*failureQueue),
		invokeCounter: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "axon_handler_invokes",
				Help: "Number of handler invokes",
			},
			[]string{"handler", "result"},
		),
		queueDepthGauge: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "axon_handler_queue_depth",
				Help: "Number of items in the handler queue",
			},
			[]string{"handler"},
		),
		handlerLatencyGauge: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "axon_handler_latency",
				Help:    "Latency of handler invocations",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"handler", "result"},
		),
		failuresPending: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "axon_handler_failure_events_pending",
				Help: "Number of failure events waiting for a client to read them",
			},
		),
		done: make(chan struct{}),
	}
	if metrics != nil {
		metrics.MustRegister(mgr.invokeCounter)
		metrics.MustRegister(mgr.queueDepthGauge)
		metrics.MustRegister(mgr.handlerLatencyGauge)
		metrics.MustRegister(mgr.failuresPending)
	}
	return mgr
}

func (s *handlerManager) Close() error {
	if s.done != nil {
		close(s.done)
		s.done = nil
	}
	return nil
}

func (s *handlerManager) IsFinished() bool {
	return s.checkFinished()
}

func (s *handlerManager) checkFinished() bool {
	for _, entry := range s.handlers {
		if !entry.IsFinished() {
			return false
		}
	}
	return true
}

func (s *handlerManager) RegisterHandler(dispatchId string, name string, timeout time.Duration, options ...*pb.HandlerOption) (string, error) {

	if s.finished {
		panic("handler manager has been closed")
	}

	for _, entry := range s.handlers {
		if entry.DispatchId() == dispatchId && entry.Name() == name {
			// ignore re-registering the same handler
			return entry.Id(), nil
		}
	}

	entry := s.createEntry(dispatchId, name, timeout, options...)
	if entry == nil {
		return "", fmt.Errorf("handler type not supported: %s", name)
	}
	s.handlers[entry.Id()] = entry
	return entry.Id(), nil
}

func (s *handlerManager) createEntry(
	dispatchId string,
	name string,
	timeout time.Duration,
	options ...*pb.HandlerOption) HandlerEntry {

	for _, option := range options {
		invoke := option.GetInvoke()
		if invoke == nil {
			continue
		}
		switch invoke.Type {
		case pb.HandlerInvokeType_RUN_NOW, pb.HandlerInvokeType_CRON_SCHEDULE, pb.HandlerInvokeType_RUN_INTERVAL:
			return newScheduledHandlerEntry(s, s.logger, dispatchId, name, timeout, s.cron, options...)

		case pb.HandlerInvokeType_WEBHOOK:
			return NewWebhookHandlerEntry(s, s.logger, dispatchId, name, timeout, options...)
		case pb.HandlerInvokeType_INVOKE:
			return NewInvokeHandlerEntry(s, s.logger, dispatchId, name, timeout, options...)
		}
	}

	if len(options) == 0 {
		return NewInvokeHandlerEntry(s, s.logger, dispatchId, name, timeout, options...)
	}

	s.logger.Error("handler type not supported", zap.String("handler", name))
	return nil
}

func (s *handlerManager) UnregisterHandler(id string) {
	entry, ok := s.handlers[id]
	if !ok {
		return
	}
	s.removeHandler(entry)
}

func (s *handlerManager) ClearHandlers(id string) {
	for _, entry := range s.handlers {
		if entry.DispatchId() == id {
			s.removeHandler(entry)
		}
	}
}

func (s *handlerManager) Start(dispatchId string) error {
	for _, entry := range s.handlers {
		if entry.DispatchId() == dispatchId {
			err := entry.Start()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *handlerManager) Stop(dispatchId string) error {
	for _, entry := range s.handlers {
		if entry.DispatchId() == dispatchId {
			entry.Close()
		}
	}
	return nil
}

func (s *handlerManager) removeHandler(entry HandlerEntry) {
	entry.Close()
	delete(s.handlers, entry.Id())
}

func (s *handlerManager) Dequeue(ctx context.Context, dispatchId string, waitTime time.Duration) (Invocable, error) {
	queue := s.getDispatchQueue(dispatchId)
	select {
	case <-time.After(waitTime):
		s.checkFinished()
		return nil, nil
	case response := <-queue:
		return response, nil
	case <-ctx.Done():
		return nil, nil
	}
}

func (s *handlerManager) Trigger(handler Invocable) error {

	handlerName := handler.GetEntry().Name()
	entry := s.handlers[handler.GetEntry().Id()]

	if entry == nil {
		s.logger.Error("handler not found", zap.String("handler", handlerName))
		return fmt.Errorf("handler not found: %s", handlerName)
	}

	message := handler

	reasonStr := message.GetReason().String()

	if !entry.IsActive() {
		s.logger.Warn("handler is not active", zap.String("handler", handlerName))
		return fmt.Errorf("cannot trigger non-started handler: %s", handlerName)
	}

	logger := s.logger.With(
		zap.String("handler-id", entry.Id()),
		zap.String("handler", handlerName),
		zap.String("reason", reasonStr),
	)

	logger.Info("Triggering handler")
	contextWithTimeout, cancel := context.WithTimeout(context.Background(), message.GetEntry().Timeout())
	handler.Start(contextWithTimeout)

	entry.OnTrigger(message.GetReason())

	startTime := time.Now()
	queue := s.getDispatchQueue(entry.DispatchId())
	select {
	case queue <- message:
	default:
		// Never block here: Trigger runs on the inbound HTTP goroutine for a
		// webhook, and a blocked send holds that request open with no timeout.
		// Cancelling completes the invocation rather than leaking it.
		cancel()
		s.queueDepthGauge.WithLabelValues(handlerName).Set(float64(len(queue)))
		s.invokeCounter.WithLabelValues(handlerName, "rejected").Inc()
		logger.Warn("Dispatch queue is full, rejecting invocation",
			zap.Int("depth", len(queue)))
		s.EmitFailure(message.ToDispatchInvoke(), &pb.InvocationFailure{
			Reason:         pb.FailureReason_NO_RESOURCES,
			Source:         pb.FailureSource_RUNTIME,
			Message:        fmt.Sprintf("dispatch queue is full (%d waiting)", len(queue)),
			CallerRejected: true,
		})
		return ErrDispatchQueueFull
	}
	s.queueDepthGauge.WithLabelValues(handlerName).Set(float64(len(queue)))

	go func() {
		defer cancel()
		<-message.Done()
		result, err := message.GetResult()
		if errors.Is(err, context.DeadlineExceeded) && message.ClaimExpired() {
			// No client picked it up in time, so it will never run.
			s.EmitFailure(message.ToDispatchInvoke(), &pb.InvocationFailure{
				Reason: pb.FailureReason_NO_RESOURCES,
				Source: pb.FailureSource_RUNTIME,
				Message: fmt.Sprintf("expired after %v waiting for a free handler",
					message.GetEntry().Timeout()),
			})
		}
		s.queueDepthGauge.WithLabelValues(handlerName).Set(float64(len(queue)))
		s.handlerLatencyGauge.WithLabelValues(handlerName, result).Observe(float64(time.Since(startTime).Milliseconds()))
		if err != nil {
			logger.Error("handler error", zap.Error(err))
			s.invokeCounter.WithLabelValues(handlerName, "error").Inc()
		} else {
			logger.Info("Handler completed", zap.Int("result-length", len(result)))
			s.invokeCounter.WithLabelValues(handlerName, "success").Inc()
		}
	}()

	return nil
}

func (s *handlerManager) SubscribeFailures(dispatchId string) {
	s.failureQueue(dispatchId, true)
}

func (s *handlerManager) failureQueue(dispatchId string, create bool) *failureQueue {
	s.failureLock.Lock()
	defer s.failureLock.Unlock()
	q, ok := s.failureQueues[dispatchId]
	if !ok && create {
		q = newFailureQueue()
		s.failureQueues[dispatchId] = q
	}
	return q
}

func (s *handlerManager) EmitFailure(invoke *pb.DispatchHandlerInvoke, failure *pb.InvocationFailure) {
	logger := s.logger.With(
		zap.String("handler", invoke.HandlerName),
		zap.String("invocation-id", invoke.InvocationId),
		zap.String("reason", failure.Reason.String()),
		zap.String("source", failure.Source.String()),
		zap.String("message", failure.Message),
	)
	q := s.failureQueue(invoke.DispatchId, false)
	if q == nil {
		logger.Warn("Invocation failed, client does not handle failure events")
		return
	}
	logger.Warn("Invocation failed, queueing failure event")
	q.push(&pb.DispatchEvent{Invoke: invoke, Failure: failure})
	s.failuresPending.Inc()
}

func (s *handlerManager) NextFailure(ctx context.Context, dispatchId string) (*pb.DispatchEvent, error) {
	ev, err := s.failureQueue(dispatchId, true).pop(ctx)
	if err != nil {
		return nil, err
	}
	s.failuresPending.Dec()
	return ev, nil
}

func (s *handlerManager) RequeueFailure(dispatchId string, ev *pb.DispatchEvent) {
	s.failureQueue(dispatchId, true).pushFront(ev)
	s.failuresPending.Inc()
}

func (s *handlerManager) Retrigger(invoke *pb.DispatchHandlerInvoke) (string, error) {
	attempt := max(invoke.Attempt, 1)
	if attempt >= maxRetryAttempts {
		return "", fmt.Errorf("%w: attempt %d of %d", ErrRetryLimit, attempt, maxRetryAttempts)
	}

	entry := s.handlers[invoke.HandlerId]
	if entry == nil {
		// The client re-registered since the invocation was sent, which gives
		// the handler a new id under the same dispatch id and name.
		for _, e := range s.handlers {
			if e.DispatchId() == invoke.DispatchId && e.Name() == invoke.HandlerName {
				entry = e
				break
			}
		}
	}
	if entry == nil {
		return "", fmt.Errorf("handler not found: %s", invoke.HandlerName)
	}

	retry := NewHandlerInvoke(entry, invoke.Reason, invoke.Args)
	retry.Attempt = attempt + 1
	if err := s.Trigger(retry); err != nil {
		return "", err
	}
	return retry.Id, nil
}

func (s *handlerManager) getDispatchQueue(DispatchId string) chan Invocable {

	queue, ok := s.dispatchQueues[DispatchId]
	if !ok {
		queue = make(chan Invocable, dispatchQueueDepth)
		s.dispatchQueues[DispatchId] = queue
	}
	return queue
}

func (s *handlerManager) ListHandlers() []HandlerEntry {
	var entries []HandlerEntry
	for _, entry := range s.handlers {
		entries = append(entries, entry)
	}
	return entries
}

func (s *handlerManager) GetByTag(tag string) HandlerEntry {
	for _, entry := range s.handlers {
		if entry.Tag() == tag && entry.IsActive() {
			return entry
		}
	}
	return nil
}

func TriggerInvoke(context context.Context, manager Manager, handlerName string, body string) (string, error) {

	entry := manager.GetByTag(handlerName)
	if entry == nil {
		return "", os.ErrNotExist
	}

	handlerInvoke := NewInvokeHandlerInvoke(entry, body)

	handlerInvoke.Start(context)

	err := manager.Trigger(handlerInvoke)

	if err != nil {
		return "", err
	}

	<-handlerInvoke.Done()
	return handlerInvoke.GetResult()
}
