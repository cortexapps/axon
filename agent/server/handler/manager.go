package handler

import (
	"context"
	"errors"
	"fmt"
	"os"
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

type Manager interface {
	RegisterHandler(dispatchId string, name string, timeout time.Duration, options ...*pb.HandlerOption) (string, error)
	UnregisterHandler(id string)
	ListHandlers() []HandlerEntry
	ClearHandlers(id string)
	Start(id string) error
	Stop(id string) error
	Trigger(handler Invocable) error
	GetByTag(tag string) HandlerEntry
	// Next hands out the next queued invocation, already moved to
	// InvokeDispatched; ones that expired while queued are skipped.
	InvocationSource[Invocable]
	Close() error
	IsFinished() bool

	// SetFailureNotifier sets where invocations that complete with a
	// FailureError are sent.
	SetFailureNotifier(notifier FailureNotifier)
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
	failures            FailureNotifier
}

func NewHandlerManager(logger *zap.Logger, cron cron.Cron, metrics *prometheus.Registry) Manager {

	mgr := &handlerManager{
		logger:              logger.Named("handler-manager"),
		handlers:            make(map[string]HandlerEntry),
		dispatchQueues:      make(map[string]chan Invocable),
		cron:                cron,
		outstandingRequests: make(map[string]Invocable),
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
		done: make(chan struct{}),
	}
	if metrics != nil {
		metrics.MustRegister(mgr.invokeCounter)
		metrics.MustRegister(mgr.queueDepthGauge)
		metrics.MustRegister(mgr.handlerLatencyGauge)
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

func (s *handlerManager) SetFailureNotifier(notifier FailureNotifier) {
	s.failures = notifier
}

func (s *handlerManager) Next(ctx context.Context, dispatchId string, waitTime time.Duration) (Invocable, error) {
	queue := s.getDispatchQueue(dispatchId)
	timer := time.NewTimer(waitTime)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			s.checkFinished()
			return nil, nil
		case invoke := <-queue:
			if invoke.SetState(InvokeDispatched) {
				return invoke, nil
			}
			// It expired while queued, and that failure has been reported;
			// handing it out now would run stale work.
		case <-ctx.Done():
			return nil, nil
		}
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

	queue := s.getDispatchQueue(entry.DispatchId())
	go s.watch(message, logger, cancel)
	select {
	case queue <- message:
	default:
		// Never block here: Trigger runs on the inbound HTTP goroutine for a
		// webhook, and a blocked send holds that request open with no timeout.
		message.SetState(InvokeRejected)
		failure := NewFailureError(pb.FailureReason_NO_RESOURCES, pb.FailureSource_RUNTIME,
			fmt.Sprintf("dispatch queue is full (%d waiting)", len(queue)))
		failure.Failure.CallerRejected = true
		message.Complete("", failure)
		logger.Warn("Dispatch queue is full, rejecting invocation",
			zap.Int("depth", len(queue)))
		return ErrDispatchQueueFull
	}
	s.queueDepthGauge.WithLabelValues(handlerName).Set(float64(len(queue)))
	return nil
}

// watch waits for an invocation to finish, however that happens, and is the
// one place a failed invocation is handed to the failure manager.
func (s *handlerManager) watch(invoke Invocable, logger *zap.Logger, cancel context.CancelFunc) {
	defer cancel()
	startTime := time.Now()
	handlerName := invoke.GetEntry().Name()
	<-invoke.Done()
	result, err := invoke.GetResult()

	s.queueDepthGauge.WithLabelValues(handlerName).Set(float64(len(s.getDispatchQueue(invoke.GetEntry().DispatchId()))))
	if invoke.State() == InvokeRejected {
		s.invokeCounter.WithLabelValues(handlerName, "rejected").Inc()
	} else {
		s.handlerLatencyGauge.WithLabelValues(handlerName, result).Observe(float64(time.Since(startTime).Milliseconds()))
		if err != nil {
			logger.Error("handler error", zap.Error(err))
			s.invokeCounter.WithLabelValues(handlerName, "error").Inc()
		} else {
			logger.Info("Handler completed", zap.Int("result-length", len(result)))
			s.invokeCounter.WithLabelValues(handlerName, "success").Inc()
		}
	}

	var failure *FailureError
	if errors.As(err, &failure) {
		if s.failures == nil {
			logger.Warn("Invocation failed, no failure manager", zap.Error(err))
			return
		}
		s.failures.Notify(invoke.ToDispatchInvoke(), failure.Failure)
	}
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

	select {
	case <-handlerInvoke.Done():
		return handlerInvoke.GetResult()
	case <-context.Done():
		return "", context.Err()
	}
}
