package vm

import (
	"encoding/json"
	"os"
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

// An older CLI reads meta.json into a struct without Arch and writes it back,
// which drops the field. The VM must still be known as x86-64 afterwards.
func TestOlderCLIRewriteOfMetaKeepsTheArchitecture(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	if err := s.CreateFrom("x86", strings.NewReader("disk"), 4, 4096, Meta{Arch: ArchAMD64}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.MetaPath("x86"))
	if err != nil {
		t.Fatal(err)
	}
	var old map[string]any
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	delete(old, "arch")
	stripped, _ := json.Marshal(old)
	if err := os.WriteFile(s.MetaPath("x86"), stripped, 0o600); err != nil {
		t.Fatal(err)
	}

	if m, ok := s.ReadMeta("x86"); !ok || m.GuestArch() != ArchAMD64 {
		t.Errorf("after an older CLI's rewrite: ReadMeta arch %q (read %t), want amd64", m.Arch, ok)
	}
	if st, err := s.Status("x86"); err != nil || st.Meta.GuestArch() != ArchAMD64 {
		t.Errorf("after an older CLI's rewrite: Status arch %q, %v; want amd64", st.Meta.Arch, err)
	}
	if ready, backup, err := s.PrepareForReuse("x86"); err != nil || !ready || backup != "" {
		t.Errorf("after an older CLI's rewrite: reuse = %v, %q, %v; want ready, not preserved as an interrupted create", ready, backup, err)
	}
	if err := s.RecordHostname("x86", "wendyos-x86"); err != nil {
		t.Fatal(err)
	}
	repaired, _ := os.ReadFile(s.MetaPath("x86"))
	if !strings.Contains(string(repaired), `"arch": "amd64"`) {
		t.Errorf("the next metadata write did not restore arch: %s", repaired)
	}
}
