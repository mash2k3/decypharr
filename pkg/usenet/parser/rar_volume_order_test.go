package parser

import (
	"reflect"
	"testing"

	nzbparser "github.com/Tensai75/nzbparser"
)

func TestRarVolumeReorder(t *testing.T) {
	tests := []struct {
		name    string
		numbers []int
		want    []int
		wantOK  bool
	}{
		{"already in order", []int{0, 1, 2, 3}, nil, false},
		{"single volume", []int{0}, nil, false},
		// Real-world case: obfuscated upload listed part52, part42, part15, ...
		{"scrambled", []int{2, 0, 3, 1}, []int{1, 3, 0, 2}, true},
		{"unknown volume", []int{2, -1, 0, 1}, nil, false},
		{"duplicate volume", []int{1, 0, 1}, nil, false},
		// A missing volume leaves a gap; still sort what we have.
		{"gap", []int{3, 0, 1}, []int{1, 2, 0}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := rarVolumeReorder(tt.numbers)
			if ok != tt.wantOK || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("rarVolumeReorder(%v) = %v, %v; want %v, %v", tt.numbers, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestParseRAR5MainVolumeNumber(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want int
	}{
		// Multi-volume flag only: first volume omits the number field.
		{"first volume", []byte{RAR5MainFlagVolume}, 0},
		{"not multi-volume", []byte{0x00}, 0},
		{"volume 52", []byte{RAR5MainFlagVolume | RAR5MainFlagVolumeNumber, 51}, 51},
		// 200 as a vint is 0xC8 0x01.
		{"multi-byte vint", []byte{RAR5MainFlagVolume | RAR5MainFlagVolumeNumber, 0xC8, 0x01}, 200},
		{"truncated number", []byte{RAR5MainFlagVolume | RAR5MainFlagVolumeNumber}, -1},
		{"empty", nil, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseRAR5MainVolumeNumber(tt.data); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseRAR4EndVolumeNumber(t *testing.T) {
	tests := []struct {
		name   string
		header rar4Header
		want   int
	}{
		{"no volume number", rar4Header{Flags: 0x0001}, -1},
		{"volume number", rar4Header{Flags: RAR4EndFlagVolNumber, Data: []byte{0x07, 0x00}}, 7},
		{"after data CRC", rar4Header{Flags: RAR4EndFlagVolNumber | RAR4EndFlagDataCRC, Data: []byte{1, 2, 3, 4, 0x2A, 0x01}}, 298},
		{"truncated", rar4Header{Flags: RAR4EndFlagVolNumber, Data: []byte{0x07}}, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseRAR4EndVolumeNumber(&tt.header); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMediaHeaderValid(t *testing.T) {
	ebml := []byte{0x1A, 0x45, 0xDF, 0xA3, 0xA3, 0x42, 0x86, 0x81}
	// Bytes from the misordered American Hostage S01E01 file: mid-stream data.
	midStream := []byte{0x5E, 0xF3, 0x4E, 0x8D, 0x70, 0x01, 0x8E, 0xF2}
	mp4 := []byte{0x00, 0x00, 0x00, 0x20, 'f', 't', 'y', 'p'}

	tests := []struct {
		name string
		file string
		head []byte
		want bool
	}{
		{"mkv ok", "a.mkv", ebml, true},
		{"mkv mid-stream", "a.MKV", midStream, false},
		{"mp4 ok", "a.mp4", mp4, true},
		{"mp4 mid-stream", "a.mp4", midStream, false},
		{"avi ok", "a.avi", []byte("RIFF\x00\x00\x00\x00"), true},
		{"mpg ok", "a.mpg", []byte{0x00, 0x00, 0x01, 0xBA}, true},
		{"unknown extension", "a.ts", midStream, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mediaHeaderValid(tt.file, tt.head); got != tt.want {
				t.Errorf("mediaHeaderValid(%q) = %v, want %v", tt.file, got, tt.want)
			}
		})
	}
}

func rarGroup(names ...string) *FileGroup {
	g := &FileGroup{}
	for _, n := range names {
		g.Files = append(g.Files, nzbparser.NzbFile{Filename: n})
	}
	return g
}

func TestApplyYencRarVolumeNames(t *testing.T) {
	t.Run("obfuscated subjects use yEnc names", func(t *testing.T) {
		// Real-world: subjects "PHMV2SbI78aIQ7wqt2.rar" etc., yEnc headers
		// carry the true part numbers.
		g := rarGroup("PHMV2SbI78aIQ7wqt2.rar", "N5TmvpKn.rar", "Z-vJ2d2U6O85.rar")
		yenc := []string{"x.part52.rar", "x.part42.rar", "x.part01.rar"}
		if !applyYencRarVolumeNames(g, yenc) {
			t.Fatal("expected rename")
		}
		for i, f := range g.Files {
			if f.Filename != yenc[i] {
				t.Errorf("file %d = %q, want %q", i, f.Filename, yenc[i])
			}
		}
	})
	t.Run("subjects already ordered", func(t *testing.T) {
		g := rarGroup("x.part01.rar", "x.part02.rar")
		if applyYencRarVolumeNames(g, []string{"y.part02.rar", "y.part01.rar"}) {
			t.Error("should keep subject names that already give an order")
		}
	})
	t.Run("missing yEnc name", func(t *testing.T) {
		g := rarGroup("a.rar", "b.rar")
		if applyYencRarVolumeNames(g, []string{"x.part01.rar", ""}) {
			t.Error("should not rename with a partial set")
		}
	})
	t.Run("yEnc names also obfuscated", func(t *testing.T) {
		g := rarGroup("a.rar", "b.rar")
		if applyYencRarVolumeNames(g, []string{"q.rar", "r.rar"}) {
			t.Error("should not rename when yEnc names give no order either")
		}
	})
	t.Run("old-style rNN names", func(t *testing.T) {
		g := rarGroup("a.rar", "b.rar", "c.rar")
		if !applyYencRarVolumeNames(g, []string{"x.r00", "x.rar", "x.r01"}) {
			t.Error("expected rename for .rar/.rNN set")
		}
	})
}
