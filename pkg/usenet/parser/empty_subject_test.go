package parser

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/Tensai75/nzbparser"
)

func TestNormalizeNZBSubjectsFromPoster(t *testing.T) {
	doc := []byte(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
 <file poster="&quot;show.part01.rar&quot; yEnc (1/1)" date="1" subject="">
  <groups><group>a.b</group></groups>
  <segments><segment bytes="9" number="1">p1@x</segment></segments>
 </file>
 <file poster="&quot;show.part02.rar&quot; yEnc (1/1)" date="1">
  <groups><group>a.b</group></groups>
  <segments><segment bytes="9" number="1">p2@x</segment></segments>
 </file>
 <file poster="uploader@example" date="1" subject="real &quot;other.mkv&quot; yEnc (1/1)">
  <groups><group>a.b</group></groups>
  <segments><segment bytes="9" number="1">o1@x</segment></segments>
 </file>
</nzb>`)
	before, err := nzbparser.Parse(bytes.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Files) != 2 {
		t.Fatalf("expected nzbparser to merge the empty-subject files, got %d files", len(before.Files))
	}
	fixed, n := normalizeNZBSubjects(doc)
	if n != 2 {
		t.Fatalf("fixed %d files, want 2", n)
	}
	after, err := nzbparser.Parse(bytes.NewReader(fixed))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range after.Files {
		names = append(names, f.Filename)
	}
	want := []string{"show.part01.rar", "show.part02.rar", "other.mkv"}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("names %v, want %v", names, want)
		}
	}
	if same, k := normalizeNZBSubjects(fixed); k != 0 || !reflect.DeepEqual(same, fixed) {
		t.Fatal("a normal NZB must be left unchanged")
	}
}

func TestRarVolumeReorderBySets(t *testing.T) {
	// Three per-episode sets, volumes scrambled.
	nums := []int{1, 0, 2, 0, 1, 0, 2}
	names := [][]string{{"E02.mkv"}, {"E01.mkv"}, {"E01.mkv"}, {"E02.mkv"}, {"E01.mkv"}, {"E03.mkv"}, {"E02.mkv"}}
	got, ok := rarVolumeReorderBySets(nums, names)
	want := []int{1, 4, 2, 3, 0, 6, 5}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v %v, want %v", got, ok, want)
	}
	if _, ok := rarVolumeReorderBySets([]int{0, 1, 0}, [][]string{{"a"}, {"a"}, {"a"}}); ok {
		t.Fatal("repeated number inside one set must not reorder")
	}
	if _, ok := rarVolumeReorderBySets([]int{0, -1}, [][]string{{"a"}, {"b"}}); ok {
		t.Fatal("unknown number must not reorder")
	}
}
