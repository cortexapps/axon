package handler

import "context"

// InvocationSource hands out work for one dispatch id. Dequeue blocks until
// there is some or ctx ends, in which case it returns ctx's error.
type InvocationSource[T any] interface {
	Dequeue(ctx context.Context, dispatchId string) (T, error)
}
