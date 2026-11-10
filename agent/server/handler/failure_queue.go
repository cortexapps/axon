package handler

import (
	"context"
	"sync"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
)

// failureQueue holds failure events for one dispatch id until its client
// reads them. It is unbounded on purpose: invocations are throttled, but a
// dropped failure is a message lost without a trace, which is the problem
// failure events exist to solve.
type failureQueue struct {
	mu     sync.Mutex
	events []*pb.DispatchEvent
	// notify has room for one wakeup, so push never blocks and a waiting
	// pop never misses an event that arrives while it is between checks.
	notify chan struct{}
}

func newFailureQueue() *failureQueue {
	return &failureQueue{notify: make(chan struct{}, 1)}
}

func (q *failureQueue) push(ev *pb.DispatchEvent) {
	q.mu.Lock()
	q.events = append(q.events, ev)
	q.mu.Unlock()
	q.wake()
}

// pushFront returns an event whose delivery failed, so it is the next one sent.
func (q *failureQueue) pushFront(ev *pb.DispatchEvent) {
	q.mu.Lock()
	q.events = append([]*pb.DispatchEvent{ev}, q.events...)
	q.mu.Unlock()
	q.wake()
}

func (q *failureQueue) wake() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// pop blocks until an event is available or ctx is done.
func (q *failureQueue) pop(ctx context.Context) (*pb.DispatchEvent, error) {
	for {
		q.mu.Lock()
		if len(q.events) > 0 {
			ev := q.events[0]
			q.events[0] = nil
			q.events = q.events[1:]
			remaining := len(q.events)
			q.mu.Unlock()
			if remaining > 0 {
				// Another reader may be waiting on the same queue.
				q.wake()
			}
			return ev, nil
		}
		q.mu.Unlock()

		select {
		case <-q.notify:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (q *failureQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.events)
}
