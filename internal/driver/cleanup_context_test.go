package driver

import (
	"context"
	"testing"
	"time"
)

type cleanupContextValueKey struct{}

func TestAccessKeyCleanupContextIsDetachedAndBounded(t *testing.T) {
	parent := context.WithValue(context.Background(), cleanupContextValueKey{}, "preserved")
	canceled, cancelParent := context.WithCancel(parent)
	cancelParent()

	cleanup, cancelCleanup := accessKeyCleanupContext(canceled)
	defer cancelCleanup()
	if err := cleanup.Err(); err != nil {
		t.Fatalf("cleanup inherited cancellation: %v", err)
	}
	if got := cleanup.Value(cleanupContextValueKey{}); got != "preserved" {
		t.Fatalf("cleanup context value = %v, want preserved", got)
	}
	deadline, ok := cleanup.Deadline()
	if !ok {
		t.Fatal("cleanup context has no deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > accessKeyCleanupTimeout {
		t.Fatalf("cleanup deadline remaining = %s, want within (0, %s]", remaining, accessKeyCleanupTimeout)
	}
}
