package manager

import "testing"

// A member of a stored zip (TorBox's zipped packs) or RAR is a byte range
// of the provider's file: the FUSE and WebDAV reads are member-relative,
// the request to the provider is not.
func TestProviderRangeShiftsIntoTheArchive(t *testing.T) {
	br := &[2]int64{1000, 1999} // a 1000-byte member at offset 1000
	cases := []struct{ start, end, wantS, wantE int64 }{
		{0, 999, 1000, 1999},    // whole member
		{10, 19, 1010, 1019},    // a read inside it
		{990, 5000, 1990, 1999}, // never past the member's last byte
	}
	for _, c := range cases {
		s, e := providerRange(br, c.start, c.end)
		if s != c.wantS || e != c.wantE {
			t.Errorf("providerRange(%d,%d) = %d,%d, want %d,%d", c.start, c.end, s, e, c.wantS, c.wantE)
		}
	}
	if s, e := providerRange(nil, 5, 9); s != 5 || e != 9 {
		t.Errorf("plain file shifted: %d,%d", s, e)
	}
}
