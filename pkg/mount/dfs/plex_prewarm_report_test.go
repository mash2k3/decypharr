package dfs

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sirrobot01/decypharr/internal/nntp"
)

// Only failures that could be proof reach the manager's proof check;
// our own timeout, cancellation, pool contention and layout mismatches don't.
func TestPrewarmFailureMayBeProof(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"missing article":  {&nntp.Error{Type: nntp.ErrorTypeArticleNotFound}, true},
		"connection error": {nntp.NewConnectionError(errors.New("reset")), true},
		"prewarm timeout":  {fmt.Errorf("read: %w", context.DeadlineExceeded), false},
		"cancelled":        {context.Canceled, false},
		"pool contention":  {nntp.ErrNoEligibleProviders, false},
		"layout mismatch":  {&nntp.Error{Type: nntp.ErrorTypeLayoutMismatch}, false},
		"nil":              {nil, false},
	} {
		if got := prewarmFailureMayBeProof(tc.err); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}
