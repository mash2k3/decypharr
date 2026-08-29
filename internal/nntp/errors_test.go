package nntp

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsPoolCapacityError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "sentinel eligible", err: ErrNoEligibleProviders, want: true},
		{name: "sentinel connection", err: ErrNoProviderConnection, want: true},
		{name: "wrapped eligible", err: fmt.Errorf("fetch: %w", ErrNoEligibleProviders), want: true},
		{name: "string fallback", err: errors.New("segment fetch: no eligible providers available"), want: true},
		{name: "article missing", err: NewProtocolError(430, "no such article"), want: false},
		{name: "timeout", err: NewTimeoutError(errors.New("deadline exceeded")), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPoolCapacityError(tc.err); got != tc.want {
				t.Fatalf("IsPoolCapacityError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
