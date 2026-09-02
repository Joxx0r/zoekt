package testutil

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestErrAfterContextConcurrentChecks(t *testing.T) {
	const (
		allowed = 16
		checks  = 64
	)
	ctx := NewErrAfterContext(allowed)
	var successful atomic.Int64
	var canceled atomic.Int64
	var wg sync.WaitGroup

	wg.Add(checks)
	for range checks {
		go func() {
			defer wg.Done()
			switch err := ctx.Err(); {
			case err == nil:
				successful.Add(1)
			case errors.Is(err, context.DeadlineExceeded):
				canceled.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := successful.Load(); got != allowed {
		t.Fatalf("successful checks = %d, want %d", got, allowed)
	}
	if got := canceled.Load(); got != checks-allowed {
		t.Fatalf("canceled checks = %d, want %d", got, checks-allowed)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Done remained open after Err reported cancellation")
	}
}
