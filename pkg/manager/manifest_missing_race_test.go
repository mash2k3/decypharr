package manager

import (
	"context"
	"testing"
	"time"
)

func TestClassifyManifestMissing(t *testing.T) {
	saved := manifestRecheckDelay
	manifestRecheckDelay = time.Millisecond
	t.Cleanup(func() { manifestRecheckDelay = saved })

	now := time.Now()
	old := now.Add(-time.Hour)
	entry := func(created time.Time, exists bool) func() (time.Time, bool) {
		return func() (time.Time, bool) { return created, exists }
	}
	missing := func() bool { return true }

	cases := []struct {
		name       string
		entry      func() (time.Time, bool)
		still      func() bool
		wantBroken bool
		wantReason string
	}{
		{"old entry, manifest lost", entry(old, true), missing, true, "usenet_manifest_missing"},
		{"entry deleted mid-sweep", entry(time.Time{}, false), missing, false, "entry_removed"},
		{"just imported", entry(now.Add(-time.Minute), true), missing, false, "usenet_manifest_pending"},
		{"manifest appeared on recheck", entry(old, true), func() bool { return false }, false, "usenet_manifest_pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broken, reason := classifyManifestMissing(context.Background(), tc.entry, now, tc.still)
			if broken != tc.wantBroken || reason != tc.wantReason {
				t.Fatalf("got (%v, %q), want (%v, %q)", broken, reason, tc.wantBroken, tc.wantReason)
			}
		})
	}

	// Deleted between the two reads.
	calls := 0
	vanishing := func() (time.Time, bool) { calls++; return old, calls == 1 }
	if broken, reason := classifyManifestMissing(context.Background(), vanishing, now, missing); broken || reason != "entry_removed" {
		t.Fatalf("entry deleted during recheck: got (%v, %q)", broken, reason)
	}
}
