package vm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseArchAcceptsCommonSpellings(t *testing.T) {
	for in, want := range map[string]Arch{
		"arm64": ArchARM64, "aarch64": ArchARM64, "ARM64": ArchARM64,
		"amd64": ArchAMD64, "x86_64": ArchAMD64, "x86-64": ArchAMD64, "x64": ArchAMD64, " AMD64 ": ArchAMD64,
	} {
		got, err := ParseArch(in)
		if err != nil || got != want {
			t.Errorf("ParseArch(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "riscv64", "arm", "386"} {
		_, err := ParseArch(bad)
		if err == nil {
			t.Errorf("ParseArch(%q) accepted an unknown architecture", bad)
			continue
		}
		if !strings.Contains(err.Error(), "arm64") || !strings.Contains(err.Error(), "amd64") {
			t.Errorf("ParseArch(%q) error %q does not name arm64 and amd64", bad, err)
		}
	}
}

func TestArchDerivedNames(t *testing.T) {
	for _, tc := range []struct {
		arch                 Arch
		board, binary, label string
	}{
		{ArchARM64, "vm-arm64", "qemu-system-aarch64", "ARM64"},
		{Arch(""), "vm-arm64", "qemu-system-aarch64", "ARM64"},
		{ArchAMD64, "vm-x86-64", "qemu-system-x86_64", "x86-64"},
	} {
		if got := tc.arch.BoardID(); got != tc.board {
			t.Errorf("Arch(%q).BoardID() = %q, want %q", tc.arch, got, tc.board)
		}
		if got := tc.arch.QEMUBinary(); got != tc.binary {
			t.Errorf("Arch(%q).QEMUBinary() = %q, want %q", tc.arch, got, tc.binary)
		}
		if got := tc.arch.Label(); got != tc.label {
			t.Errorf("Arch(%q).Label() = %q, want %q", tc.arch, got, tc.label)
		}
	}
	for arch, want := range map[Arch]bool{"": true, ArchARM64: true, ArchAMD64: true, "riscv64": false, "x86_64": false} {
		if got := arch.Valid(); got != want {
			t.Errorf("Arch(%q).Valid() = %t, want %t", arch, got, want)
		}
	}
}

func TestArchForBoardOnlyKnowsTheTwoVMBoards(t *testing.T) {
	for board, want := range map[string]Arch{"vm-arm64": ArchARM64, "vm-x86-64": ArchAMD64} {
		got, ok := ArchForBoard(board)
		if !ok || got != want {
			t.Errorf("ArchForBoard(%q) = %q, %t; want %q, true", board, got, ok, want)
		}
	}
	for _, board := range []string{"generic-x86-64", "x86_64", "vm-arm64-untrusted", ""} {
		if got, ok := ArchForBoard(board); ok {
			t.Errorf("ArchForBoard(%q) = %q, true; want not ok", board, got)
		}
	}
}

func TestMetaWithoutArchIsARM64(t *testing.T) {
	var old Meta
	if err := json.Unmarshal([]byte(`{"name":"old"}`), &old); err != nil {
		t.Fatal(err)
	}
	if got := old.GuestArch(); got != ArchARM64 {
		t.Errorf("meta without arch: GuestArch() = %q, want arm64", got)
	}
	var x86 Meta
	if err := json.Unmarshal([]byte(`{"name":"n","arch":"amd64"}`), &x86); err != nil {
		t.Fatal(err)
	}
	if got := x86.GuestArch(); got != ArchAMD64 {
		t.Errorf("meta with arch amd64: GuestArch() = %q, want amd64", got)
	}
	data, err := json.Marshal(Meta{Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"arch"`) {
		t.Errorf("a Meta without Arch serialises an arch key: %s", data)
	}
}

func TestDefaultArchIsARM64OnEveryHost(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, goarch := range []string{"amd64", "arm64"} {
			if got := DefaultArchFor(goos, goarch); got != ArchARM64 {
				t.Errorf("DefaultArchFor(%s, %s) = %q, want arm64", goos, goarch, got)
			}
		}
	}
	if got := DefaultArch(); got != ArchARM64 {
		t.Errorf("DefaultArch() = %q, want arm64", got)
	}
}
