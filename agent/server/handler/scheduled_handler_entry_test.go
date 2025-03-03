package handler

import (
	"context"
	"os"
	"testing"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon/server/cron"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestApplyOption_RunNow(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	manager := NewHandlerManager(logger, cron)
	entry := newScheduledHandlerEntry(manager, logger, "1", "handler1", defaultTimeout, cron).(*ScheduledHandlerEntry)

	option := &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{
				Type: pb.HandlerInvokeType_RUN_NOW,
			},
		},
	}

	err := entry.applyOption(option)
	require.NoError(t, err)
}

func TestApplyOption_RunInterval(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	manager := newFakeManager()
	option := &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{
				Type:  pb.HandlerInvokeType_RUN_INTERVAL,
				Value: "1ms",
			},
		},
	}

	entry := newScheduledHandlerEntry(manager, logger, "1", "handler1", defaultTimeout, cron, option).(*ScheduledHandlerEntry)
	manager.handlers = append(manager.handlers, entry)
	err := entry.Start()

	require.Nil(t, entry.LastInvoked())
	require.NoError(t, err)

	time.Sleep(10 * time.Millisecond)

	require.True(t, len(manager.triggered) > 0)
	require.Equal(t, "handler1", manager.triggered[0].Name)

	require.True(t, entry.IsActive())
	entry.Close()
	require.False(t, entry.IsActive())

	require.NotNil(t, entry.LastInvoked())

}

func TestApplyOption_RunInterval_InvalidDuration(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	manager := NewHandlerManager(logger, cron)
	entry := newScheduledHandlerEntry(manager, logger, "1", "handler1", defaultTimeout, cron).(*ScheduledHandlerEntry)

	option := &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{
				Type:  pb.HandlerInvokeType_RUN_INTERVAL,
				Value: "invalid",
			},
		},
	}

	err := entry.applyOption(option)
	require.Error(t, err)

}

func TestApplyOption_CronSchedule(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := &fakeCron{
		handlers: make(map[string]func()),
	}
	manager := &fakeManager{
		triggered: make([]HandlerInvoke, 0),
	}

	option := &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{
				Type:  pb.HandlerInvokeType_CRON_SCHEDULE,
				Value: "1 * * * *",
			},
		},
	}

	entry := newScheduledHandlerEntry(
		manager, logger, "1", "handler1", defaultTimeout, cron, option).(*ScheduledHandlerEntry)

	err := entry.Start()
	require.NoError(t, err)
	require.True(t, entry.IsActive())

	time.Sleep(10 * time.Millisecond)
	require.True(t, len(manager.triggered) > 0)
	require.Equal(t, "handler1", manager.triggered[0].Name)

	entry.Close()
	require.False(t, entry.IsActive())

	// close is async, wait for it to finish
	for i := 0; i < 10; i++ {
		time.Sleep(10 * time.Millisecond)
		if len(cron.handlers) == 0 {
			return
		}
	}
	require.Len(t, cron.handlers, 0)

}

func TestApplyOption_CronSchedule_Invalid(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	manager := NewHandlerManager(logger, cron)
	entry := newScheduledHandlerEntry(manager, logger, "1", "handler1", defaultTimeout, cron).(*ScheduledHandlerEntry)

	option := &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{
				Type:  pb.HandlerInvokeType_CRON_SCHEDULE,
				Value: "1 *",
			},
		},
	}

	err := entry.applyOption(option)
	require.Error(t, err)
}

func TestIsFinishedOnRunNowOnly(t *testing.T) {

	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	manager := NewHandlerManager(logger, cron)

	option := &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{
				Type: pb.HandlerInvokeType_RUN_NOW,
			},
		},
	}

	entry := newScheduledHandlerEntry(manager, logger, "1", "handler1", defaultTimeout, cron, option).(*ScheduledHandlerEntry)

	require.False(t, entry.IsFinished())

	err := entry.applyOption(option)
	require.NoError(t, err)

	require.True(t, entry.IsFinished())

}

func TestIsFinishedOnRunNowAndInterval(t *testing.T) {

	logger, _ := zap.NewDevelopment()
	cron := cron.New()
	manager := NewHandlerManager(logger, cron)

	option1 := &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{
				Type: pb.HandlerInvokeType_RUN_NOW,
			},
		},
	}

	option2 := &pb.HandlerOption{
		Option: &pb.HandlerOption_Invoke{
			Invoke: &pb.HandlerInvokeOption{
				Type:  pb.HandlerInvokeType_RUN_INTERVAL,
				Value: "1ms",
			},
		},
	}

	entry := newScheduledHandlerEntry(manager, logger, "1", "handler1", defaultTimeout, cron, option1, option2).(*ScheduledHandlerEntry)

	require.False(t, entry.IsFinished())

	err := entry.applyOption(option1)
	require.NoError(t, err)

	require.False(t, entry.IsFinished())

	err = entry.applyOption(option2)
	require.NoError(t, err)

	require.False(t, entry.IsFinished())

}

type fakeManager struct {
	triggered []HandlerInvoke
	handlers  []HandlerEntry
}

func newFakeManager() *fakeManager {
	return &fakeManager{
		triggered: make([]HandlerInvoke, 0),
		handlers:  make([]HandlerEntry, 0),
	}
}

func (fhm *fakeManager) RegisterHandler(id string, name string, timeout time.Duration, options ...*pb.HandlerOption) (string, error) {
	panic("not implemented") // TODO: Implement
}

func (fhm *fakeManager) UnregisterHandler(id string) {
	panic("not implemented") // TODO: Implement
}

func (fhm *fakeManager) ListHandlers() []HandlerEntry {
	panic("not implemented") // TODO: Implement
}

func (fhm *fakeManager) GetByTag(t string) HandlerEntry {
	panic("not implemented") // TODO: Implement
}

func (fhm *fakeManager) ClearHandlers(id string) {
	panic("not implemented") // TODO: Implement
}

func (fhm *fakeManager) Trigger(handler HandlerInvoke) error {
	fhm.triggered = append(fhm.triggered, handler)
	for _, h := range fhm.handlers {
		if h.Id() == handler.Id {
			h.OnTrigger(handler.Reason)
			return nil
		}
	}
	return os.ErrNotExist
}

func (fhm *fakeManager) Dequeue(ctx context.Context, id string, waitTime time.Duration) (*pb.DispatchHandlerInvoke, error) {
	panic("not implemented") // TODO: Implement
}

func (fhm *fakeManager) Start(id string) error {
	panic("not implemented") // TODO: Implement
}

func (fhm *fakeManager) Stop(id string) error {
	panic("not implemented") // TODO: Implement
}

func (fhm *fakeManager) Close() error {
	return nil
}

func (fhm *fakeManager) IsFinished() bool {
	return false
}

type fakeCron struct {
	handlers map[string]func()
}

func (fc *fakeCron) Add(spec string, cmd func()) (string, error) {
	id := "1"
	fc.handlers[id] = cmd
	go cmd()
	return id, nil
}

func (fc *fakeCron) Remove(id string) {
	delete(fc.handlers, id)
}
