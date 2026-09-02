package testutil

import (
	"context"
	"sync"
)

// NewErrAfterContext returns a context whose Err method starts reporting
// DeadlineExceeded after the requested number of successful checks.
func NewErrAfterContext(allowed int) context.Context {
	return &errAfterContext{
		Context: context.Background(),
		allowed: allowed,
		done:    make(chan struct{}),
	}
}

type errAfterContext struct {
	context.Context
	allowed int
	mu      sync.Mutex
	calls   int
	done    chan struct{}
	err     error
}

func (c *errAfterContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err != nil {
		return c.err
	}
	c.calls++
	if c.calls > c.allowed {
		c.err = context.DeadlineExceeded
		close(c.done)
	}
	return c.err
}

func (c *errAfterContext) Done() <-chan struct{} {
	return c.done
}
