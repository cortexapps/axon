package handler

import (
	"context"
	"strconv"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/server/cron"
	"go.uber.org/zap"
)

// Manager manages a single handler and is responsible for triggering its invocations
type ScheduledHandlerEntry struct {
	HandlerEntry
	handler  func(context.Context, *pb.DispatchRequest) (*pb.DispatchHandlerInvoke, error)
	manager  Manager
	cronId   string
	logger   *zap.Logger
	cron     cron.Cron
	done     chan struct{}
	finished bool
}

func NewScheduledHandlerInvoke(entry HandlerEntry, reason pb.HandlerInvokeType) HandlerInvoke {
	invoke := HandlerInvoke{
		Id:      entry.Id(),
		Name:    entry.Name(),
		Reason:  reason,
		Args:    make(map[string]string),
		Timeout: entry.Timeout(),
	}

	if invoke.Timeout == 0 {
		invoke.Timeout = defaultTimeout
	}
	return invoke
}

func newScheduledHandlerEntry(
	manager Manager,
	logger *zap.Logger,
	dispatchId string,
	name string,
	timeout time.Duration,
	cron cron.Cron,
	options ...*pb.HandlerOption,
) HandlerEntry {
	handler := func(context.Context, *pb.DispatchRequest) (*pb.DispatchHandlerInvoke, error) {
		return nil, nil
	}
	entry := &ScheduledHandlerEntry{
		HandlerEntry: NewHandlerEntry(
			"",
			dispatchId,
			name,
			timeout,
			options...,
		),
		logger:  logger,
		handler: handler,
		manager: manager,
		cron:    cron,
	}
	return entry
}

func (h *ScheduledHandlerEntry) Start() error {
	if h.done != nil {
		return nil
	}
	h.done = make(chan struct{})
	h.logger.Info("Starting handler", zap.String("handler", h.Name()))
	fail := false

	defer func() {
		if fail {
			h.Close()
		}
	}()
	h.HandlerEntry.Start()

	for _, option := range h.Options() {
		if err := h.applyOption(option); err != nil {
			fail = true
			return err
		}
	}
	return nil
}

func (h *ScheduledHandlerEntry) Close() {
	if h.done == nil {
		return
	}
	h.logger.Info("Stopping handler", zap.String("handler", h.Name()))
	close(h.done)
	h.done = nil
	h.HandlerEntry.Close()
}

func (h *ScheduledHandlerEntry) applyOption(opt *pb.HandlerOption) error {

	option := opt.GetInvoke()
	if option == nil {
		return nil
	}

	switch option.Type {
	case pb.HandlerInvokeType_RUN_NOW:
		h.logger.Info("Running handler due to RUN_NOW", zap.String("handler", h.Name()))
		invoke := NewScheduledHandlerInvoke(h, pb.HandlerInvokeType_RUN_NOW)
		h.manager.Trigger(invoke)
		if h.isSingleTrigger() {
			h.finished = true
		}
	case pb.HandlerInvokeType_RUN_INTERVAL:

		duration, err := time.ParseDuration(option.Value)
		if err != nil {

			asInt, err2 := strconv.Atoi(option.Value)
			if err2 == nil {
				duration = time.Duration(asInt) * time.Second
			} else {
				h.logger.Error("failed to parse duration", zap.Error(err))
				return err
			}
		}

		h.logger.Info("Registering handler with RUN_INTERVAL", zap.String("handler", h.Name()), zap.Duration("interval", duration))
		go func() {
			timer := time.NewTicker(duration)

			for {
				select {
				case <-timer.C:
					invoke := NewScheduledHandlerInvoke(h, pb.HandlerInvokeType_RUN_INTERVAL)
					h.manager.Trigger(invoke)
				case <-h.done:
					return
				}
			}

		}()
	case pb.HandlerInvokeType_CRON_SCHEDULE:
		h.logger.Info("Registering handler with CRON_SCHEDULE", zap.String("handler", h.Name()), zap.String("schedule", option.Value))
		id, err := h.cron.Add(option.Value, func() {
			invoke := NewScheduledHandlerInvoke(h, pb.HandlerInvokeType_CRON_SCHEDULE)
			h.manager.Trigger(invoke)
		})
		if err != nil {
			h.logger.Error("failed to add cron job", zap.Error(err))
			return err
		}
		go func() {
			<-h.done
			h.cron.Remove(id)
		}()
		h.cronId = id
	}

	return nil
}

func (h *ScheduledHandlerEntry) isSingleTrigger() bool {
	options := h.Options()
	invokeOptions := make([]*pb.HandlerInvokeOption, 0, len(options))
	for _, opt := range options {
		if invoke := opt.GetInvoke(); invoke != nil {
			invokeOptions = append(invokeOptions, invoke)
		}
	}

	return len(invokeOptions) == 1 && invokeOptions[0].Type == pb.HandlerInvokeType_RUN_NOW
}

func (h *ScheduledHandlerEntry) IsFinished() bool {
	return h.finished
}
