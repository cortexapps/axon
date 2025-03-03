package snykbroker

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCountEventsWithinWindow_NoEvents(t *testing.T) {
	tracker := newEventTracker()
	count := tracker.CountEventsWithinWindow(1 * time.Minute)
	require.Equal(t, 0, count)
}

func TestCountEventsWithinWindow_AllEventsWithinWindow(t *testing.T) {
	tracker := newEventTracker()
	tracker.AddEvent()
	tracker.AddEvent()
	tracker.AddEvent()

	count := tracker.CountEventsWithinWindow(1 * time.Minute)
	require.Equal(t, 3, count)
}

func TestCountEventsWithinWindow_SomeEventsWithinWindow(t *testing.T) {
	tracker := newEventTracker()
	tracker.AddEvent()
	time.Sleep(5 * time.Millisecond)
	tracker.AddEvent()
	time.Sleep(5 * time.Millisecond)
	tracker.AddEvent()

	count := tracker.CountEventsWithinWindow(5 * time.Millisecond)
	require.Equal(t, 1, count)
}

func TestCountEventsWithinWindow_NoEventsWithinWindow(t *testing.T) {
	tracker := newEventTracker()
	tracker.AddEvent()
	tracker.AddEvent()
	tracker.AddEvent()

	time.Sleep(10 * time.Millisecond)

	count := tracker.CountEventsWithinWindow(5 * time.Millisecond)
	require.Equal(t, 0, count)
}

func TestCountEventsWithinWindow_EventsCleanup(t *testing.T) {
	tracker := newEventTracker()
	tracker.AddEvent()
	time.Sleep(5 * time.Millisecond)
	tracker.AddEvent()
	time.Sleep(5 * time.Millisecond)
	tracker.AddEvent()

	count := tracker.CountEventsWithinWindow(5 * time.Millisecond)
	require.Equal(t, 1, count)
	time.Sleep(5 * time.Millisecond)

	count = tracker.CountEventsWithinWindow(5 * time.Millisecond)
	require.Equal(t, 0, count)

}
