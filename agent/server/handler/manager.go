package handler

import (
	"context"
	"fmt"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/server/cron"
	"go.uber.org/zap"
)

type Manager interface {
	RegisterHandler(dispatchId string, name string, timeout time.Duration, options ...*pb.HandlerOption) (string, error)
	UnregisterHandler(id string)
	ListHandlers() []HandlerEntry
	ClearHandlers(id string)
	Start(id string) error
	Stop(id string) error
	Trigger(handler HandlerInvoke) error
	GetByTag(tag string) HandlerEntry
	Dequeue(ctx context.Context, id string, waitTime time.Duration) (*pb.DispatchHandlerInvoke, error)
	Close() error
	IsFinished() bool
}

type handlerManager struct {
	logger         *zap.Logger
	dispatchQueues map[string]chan *pb.DispatchHandlerInvoke
	handlers       map[string]HandlerEntry
	cron           cron.Cron
	done           chan struct{}
	finished       bool
}

func NewHandlerManager(logger *zap.Logger, cron cron.Cron) Manager {
	mgr := &handlerManager{
		logger:         logger.Named("handler-manager"),
		handlers:       make(map[string]HandlerEntry),
		dispatchQueues: make(map[string]chan *pb.DispatchHandlerInvoke),
		cron:           cron,
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
		}
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

func (s *handlerManager) Dequeue(ctx context.Context, dispatchId string, waitTime time.Duration) (*pb.DispatchHandlerInvoke, error) {
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

func (s *handlerManager) Trigger(handler HandlerInvoke) error {
	reasonStr := handler.Reason.String()

	entry := s.handlers[handler.Id]

	if entry == nil {
		s.logger.Error("handler not found", zap.String("handler", handler.Name))
		return fmt.Errorf("handler not found: %s", handler.Name)
	}

	if !entry.IsActive() {
		s.logger.Warn("handler is not active", zap.String("handler", handler.Name))
		return fmt.Errorf("cannot trigger non-started handler: %s", handler.Name)
	}

	s.logger.Info("Triggering handler",
		zap.String("handler-id", handler.Id),
		zap.String("handler", handler.Name),
		zap.String("reason", reasonStr),
	)

	entry.OnTrigger(handler.Reason)

	queue := s.getDispatchQueue(entry.DispatchId())
	queue <- &pb.DispatchHandlerInvoke{
		DispatchId:  entry.DispatchId(),
		HandlerId:   entry.Id(),
		HandlerName: entry.Name(),
		Reason:      handler.Reason,
		Args:        handler.Args,
		TimeoutMs:   int32(entry.Timeout().Milliseconds()),
	}
	return nil
}

func (s *handlerManager) getDispatchQueue(DispatchId string) chan *pb.DispatchHandlerInvoke {

	queue, ok := s.dispatchQueues[DispatchId]
	if !ok {
		queue = make(chan *pb.DispatchHandlerInvoke, 100)
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
		if entry.Tag() == tag {
			return entry
		}
	}
	return nil
}
