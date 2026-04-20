package configpartition

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.uber.org/zap"
)

// makeELF writes a minimal ELF header to a temp file and returns its path.
// machine is the e_machine value (little-endian uint16 at bytes 18-19).
// class is EI_CLASS (byte 4): 2 = 64-bit.
// data is EI_DATA (byte 5): 1 = little-endian, 0 = invalid.
func makeELF(t *testing.T, class byte, machine uint16) string {
	t.Helper()
	return makeELFWithData(t, class, 1, machine)
}

func makeELFWithData(t *testing.T, class, data byte, machine uint16) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-elf-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	buf := make([]byte, 20)
	buf[0], buf[1], buf[2], buf[3] = 0x7f, 'E', 'L', 'F'
	buf[4] = class
	buf[5] = data
	buf[18] = byte(machine)
	buf[19] = byte(machine >> 8)
	if _, err := f.Write(buf); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func TestValidateELF_ValidArch(t *testing.T) {
	machine := elfMachineByArch[runtime.GOARCH]
	path := makeELF(t, 2, machine)
	if err := validateELF(path); err != nil {
		t.Fatalf("expected no error for valid ELF, got: %v", err)
	}
}

func TestValidateELF_BadMagic(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "not-elf-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("not an ELF file at all!!!!!")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := validateELF(f.Name()); err == nil {
		t.Fatal("expected error for non-ELF file")
	}
}

func TestValidateELF_32Bit(t *testing.T) {
	machine := elfMachineByArch[runtime.GOARCH]
	path := makeELF(t, 1, machine) // class=1 → 32-bit
	if err := validateELF(path); err == nil {
		t.Fatal("expected error for 32-bit ELF")
	}
}

func TestValidateELF_BigEndian(t *testing.T) {
	machine := elfMachineByArch[runtime.GOARCH]
	path := makeELFWithData(t, 2, 2, machine) // data=2 → big-endian
	if err := validateELF(path); err == nil {
		t.Fatal("expected error for big-endian ELF")
	}
}

func TestValidateELF_WrongArch(t *testing.T) {
	// Use the "other" architecture value so the test works on both arm64 and amd64.
	wrongMachine := elfMachineByArch["arm64"]
	if runtime.GOARCH == "arm64" {
		wrongMachine = elfMachineByArch["amd64"]
	}
	path := makeELF(t, 2, wrongMachine)
	if err := validateELF(path); err == nil {
		t.Fatal("expected error for wrong-arch ELF")
	}
}

func TestValidateELF_TooShort(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "short-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0x7f, 'E', 'L', 'F'}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := validateELF(f.Name()); err == nil {
		t.Fatal("expected error for truncated ELF header")
	}
}

func TestParseINI_BasicSections(t *testing.T) {
	data := []byte("[wifi]\nssid = MyNet\npassword = hunter2\n")
	got := parseINI(data)
	if got["wifi"]["ssid"] != "MyNet" {
		t.Errorf("ssid = %q; want %q", got["wifi"]["ssid"], "MyNet")
	}
	if got["wifi"]["password"] != "hunter2" {
		t.Errorf("password = %q; want %q", got["wifi"]["password"], "hunter2")
	}
}

func TestParseINI_Comments(t *testing.T) {
	data := []byte("# top comment\n[wifi]\n; inline comment\nssid = Net\n")
	got := parseINI(data)
	if got["wifi"]["ssid"] != "Net" {
		t.Errorf("ssid = %q; want %q", got["wifi"]["ssid"], "Net")
	}
	if _, ok := got[""]; ok {
		t.Error("should not have empty section key")
	}
}

func TestParseINI_ValueWithEquals(t *testing.T) {
	data := []byte("[wifi]\npassword = p@ss=word\n")
	got := parseINI(data)
	if got["wifi"]["password"] != "p@ss=word" {
		t.Errorf("password = %q; want %q", got["wifi"]["password"], "p@ss=word")
	}
}

func TestParseINI_Empty(t *testing.T) {
	got := parseINI([]byte(""))
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
}

func TestApplyBinaryUpdate_ValidBinary(t *testing.T) {
	dir := t.TempDir()
	installDir := t.TempDir()

	// Write a valid ELF to the config dir using the existing makeELF helper.
	machine := elfMachineByArch[runtime.GOARCH]
	src := filepath.Join(dir, "wendy-agent")
	// Copy the ELF bytes from makeELF into src manually so we can place it at the right name.
	elfPath := makeELF(t, 2, machine)
	data, err := os.ReadFile(elfPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, data, 0o755); err != nil {
		t.Fatal(err)
	}

	installPath := filepath.Join(installDir, "wendy-agent")
	logger, _ := zap.NewDevelopment()

	updated := applyBinaryUpdate(logger, dir, installPath)
	if !updated {
		t.Fatal("expected applyBinaryUpdate to return true")
	}

	info, err := os.Stat(installPath)
	if err != nil {
		t.Fatalf("installed binary not found: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary mode = %o; want exec bits set", info.Mode().Perm())
	}
	got, err := os.ReadFile(installPath)
	if err != nil {
		t.Fatalf("reading installed binary: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("installed binary content does not match source")
	}

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source binary should be deleted after install")
	}
}

func TestApplyBinaryUpdate_InvalidELF(t *testing.T) {
	dir := t.TempDir()
	installDir := t.TempDir()

	src := filepath.Join(dir, "wendy-agent")
	if err := os.WriteFile(src, []byte("not an elf"), 0o755); err != nil {
		t.Fatal(err)
	}

	installPath := filepath.Join(installDir, "wendy-agent")
	logger, _ := zap.NewDevelopment()

	updated := applyBinaryUpdate(logger, dir, installPath)
	if updated {
		t.Fatal("expected applyBinaryUpdate to return false for invalid ELF")
	}

	// Source should be deleted (bad binary, don't retry).
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("invalid binary should be deleted from config partition")
	}
}

func TestApplyBinaryUpdate_NoBinary(t *testing.T) {
	dir := t.TempDir()
	installDir := t.TempDir()
	installPath := filepath.Join(installDir, "wendy-agent")
	logger, _ := zap.NewDevelopment()

	updated := applyBinaryUpdate(logger, dir, installPath)
	if updated {
		t.Fatal("expected false when no binary present")
	}
}

func TestApplyWiFiConfig_DeletesFileAfterApply(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "wendy.conf")
	// Empty ssid so nmcli is never called, but the file deletion path is exercised.
	if err := os.WriteFile(conf, []byte("[wifi]\nssid = \n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logger, _ := zap.NewDevelopment()
	applyWiFiConfig(logger, dir)

	if _, err := os.Stat(conf); !os.IsNotExist(err) {
		t.Error("wendy.conf should be deleted after applyWiFiConfig")
	}
}

func TestApplyWiFiConfig_NoFile(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	// Should return without panic when file doesn't exist.
	applyWiFiConfig(logger, t.TempDir())
}
