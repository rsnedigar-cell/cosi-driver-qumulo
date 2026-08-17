package driver

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestToStatusPreservesContextCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code codes.Code
	}{
		{name: "canceled", err: context.Canceled, code: codes.Canceled},
		{name: "deadline", err: context.DeadlineExceeded, code: codes.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := status.Code(toStatus(fmt.Errorf("wrapped: %w", tc.err))); got != tc.code {
				t.Fatalf("code=%s, want %s", got, tc.code)
			}
		})
	}
}
