package snykbroker

import (
	"sync"
	"time"
)

// eventTracker tracks the timestamps of events
type eventTracker struct {
	events []time.Time
	lock   sync.Mutex
}

// NeweventTracker creates a new eventTracker
func newEventTracker() *eventTracker {
	return &eventTracker{
		events: make([]time.Time, 0),
	}
}

// AddEvent adds a new event with the current timestamp
func (et *eventTracker) AddEvent() {
	et.lock.Lock()
	defer et.lock.Unlock()
	et.events = append(et.events, time.Now())
}

// CountEventsWithinWindow counts the number of events within the specified time window
func (et *eventTracker) CountEventsWithinWindow(window time.Duration) int {
	et.lock.Lock()
	defer et.lock.Unlock()
	now := time.Now()
	count := 0
	invalidBeforeIndex := -1
	for i, eventTime := range et.events {
		if now.Sub(eventTime) <= window {
			count++
		} else if invalidBeforeIndex == -1 {
			invalidBeforeIndex = i
		}
	}
	if invalidBeforeIndex >= 0 {
		et.events = et.events[invalidBeforeIndex:]
	}
	return count
}
