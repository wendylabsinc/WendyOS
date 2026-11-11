package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFriendlyDeviceNameUsesLiteralRenameThenInstalledName(t *testing.T) {
	dir := t.TempDir()
	explicit, installed := filepath.Join(dir, "hostname"), filepath.Join(dir, "device-name")
	paths := []string{explicit, installed}
	write := func(path, value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(installed, "jetson--a\n")
	if got := friendlyDeviceName(paths, "wendyos-jetson--a"); got != "jetson--a" {
		t.Fatalf("installed name = %q", got)
	}
	if err := os.Symlink(installed, installed+"-link"); err == nil {
		if got := friendlyDeviceName([]string{installed + "-link"}, "fallback"); got != "jetson--a" {
			t.Fatalf("persistent-name symlink = %q", got)
		}
	}
	write(explicit, "wendyos-literal-name\n")
	if got := friendlyDeviceName(paths, "wendyos-jetson--a"); got != "wendyos-literal-name" {
		t.Fatalf("explicit prefix was rewritten: %q", got)
	}
	write(explicit, "\n")
	if got := friendlyDeviceName(paths, "generated-host"); got != "jetson--a" {
		t.Fatalf("empty explicit name hid installed label: %q", got)
	}
	write(installed, "invalid name\n")
	if got := friendlyDeviceName(paths, "Fallback-Host"); got != "fallback-host" {
		t.Fatalf("fallback = %q", got)
	}
	if got := friendlyDeviceName(paths, "invalid.host"); got != "" {
		t.Fatalf("invalid fallback became an alias: %q", got)
	}
}

func TestFriendlyDeviceNameRejectsInvalidAndOversizeFiles(t *testing.T) {
	for _, name := range []string{"-bad", "bad-", "bad.name", "bad_name", "Kelvin", strings.Repeat("a", 64), "device-22222222-2222-4222-8222-222222222222"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "name")
			if err := os.WriteFile(path, []byte(name), 0600); err != nil {
				t.Fatal(err)
			}
			if _, ok := readFriendlyDeviceName(path); ok {
				t.Fatal("invalid source was admitted")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "name")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, ok := readFriendlyDeviceName(path); ok {
		t.Fatal("directory was read as a name")
	}
	if err := os.Symlink(path, path+"-link"); err == nil {
		if _, ok := readFriendlyDeviceName(path + "-link"); ok {
			t.Fatal("symlink was read as a name")
		}
	}
}
