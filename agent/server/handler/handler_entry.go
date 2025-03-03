package handler

import (
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/google/uuid"
)

const defaultTimeout = 5 * time.Minute

type HandlerEntry interface {
	Id() string
	DispatchId() string
	Name() string
	Tag() string
	Options() []*pb.HandlerOption
	Timeout() time.Duration

	IsActive() bool
	IsFinished() bool
	LastInvoked() *time.Time
	OnTrigger(reason pb.HandlerInvokeType)

	Start() error
	Close()
}

func NewHandlerEntry(
	id string,
	dispatchId string,
	name string,
	timeout time.Duration,
	options ...*pb.HandlerOption,
) HandlerEntry {
	if id == "" {
		id = uuid.New().String()
	}
	entry := &HandlerEntryBase{
		id:         id,
		dispatchId: dispatchId,
		name:       name,
		options:    options,
		timeout:    timeout,
	}

	return entry
}

type HandlerEntryBase struct {
	id          string
	dispatchId  string
	name        string
	options     []*pb.HandlerOption
	isActive    bool
	lastInvoked *time.Time
	timeout     time.Duration
}

func (h *HandlerEntryBase) Id() string {
	return h.id
}

func (h *HandlerEntryBase) DispatchId() string {
	return h.dispatchId
}

func (h *HandlerEntryBase) Name() string {
	return h.name
}

func (h *HandlerEntryBase) Tag() string {
	return h.Name()
}

func (h *HandlerEntryBase) Options() []*pb.HandlerOption {
	return h.options
}

func (h *HandlerEntryBase) Start() error {
	h.isActive = true
	return nil
}

func (h *HandlerEntryBase) Close() {
	h.isActive = false
}

func (h *HandlerEntryBase) IsActive() bool {
	return h.isActive
}

func (h *HandlerEntryBase) IsFinished() bool {
	return false
}

func (h *HandlerEntryBase) LastInvoked() *time.Time {
	return h.lastInvoked
}

func (h *HandlerEntryBase) OnTrigger(reason pb.HandlerInvokeType) {
	now := time.Now()
	h.lastInvoked = &now
}

func (h *HandlerEntryBase) Timeout() time.Duration {

	if h.timeout == 0 {
		return defaultTimeout
	}

	return h.timeout
}
