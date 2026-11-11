package handler

import (
	"context"
	"time"
)

// InvocationSource hands out work for one dispatch id. Next waits up to wait
// and returns the zero value when nothing arrived in that time.
type InvocationSource[T comparable] interface {
	Next(ctx context.Context, dispatchId string, wait time.Duration) (T, error)
}
