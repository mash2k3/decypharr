package server

import (
	"strings"
	"testing"
)

// The checks that run before the manager is asked whether the id is taken.
func TestCheckKeepIDRejectsBadRequests(t *testing.T) {
	s := &Server{}
	if err := s.checkKeepID("95af6933-9d46-487a-96a1-efac7d2b4113", 2); err == nil || !strings.Contains(err.Error(), "single NZB") {
		t.Fatalf("two NZBs with one id: %v", err)
	}
	for _, id := range []string{"rd-123", "not a uuid", "95af69339d46487a96a1efac7d2b4113"} {
		if err := s.checkKeepID(id, 1); err == nil || !strings.Contains(err.Error(), "not a UUID") {
			t.Fatalf("%q: %v", id, err)
		}
	}
}
