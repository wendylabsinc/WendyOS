package localkey

import (
	"bytes"
	"crypto/x509"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadOrCreateIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", FileName)
	first, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := PublicKeyDER(first)
	b, _ := PublicKeyDER(second)
	if !bytes.Equal(a, b) {
		t.Fatal("second call generated a new key")
	}
	if _, err := x509.ParsePKIXPublicKey(a); err != nil {
		t.Fatalf("public key does not parse: %v", err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("key mode %#o, want 0600", fi.Mode().Perm())
		}
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".local-operator-key-*")); len(left) > 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

func TestLoadRefusesAReadableKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	path := filepath.Join(t.TempDir(), FileName)
	if _, err := LoadOrCreate(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(path); err == nil {
		t.Fatal("loaded a world-readable key")
	}
}

// Under sudo (the Thor re-exec, or a user's own sudo) the key must never be
// created: it would be root's. An existing key still loads.
func TestLoadOrCreateRefusesToCreateUnderSudo(t *testing.T) {
	orig := geteuid
	t.Cleanup(func() { geteuid = orig })
	path := filepath.Join(t.TempDir(), FileName)

	geteuid = func() int { return 0 }
	t.Setenv("SUDO_UID", "1000")
	if _, err := LoadOrCreate(path); err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("err = %v, want a refusal under sudo", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a key was created under sudo")
	}

	geteuid = orig
	t.Setenv("SUDO_UID", "")
	created, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	geteuid = func() int { return 0 }
	t.Setenv("SUDO_UID", "1000")
	loaded, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("loading an existing key under sudo: %v", err)
	}
	a, _ := PublicKeyDER(created)
	b, _ := PublicKeyDER(loaded)
	if !bytes.Equal(a, b) {
		t.Fatal("loaded a different key under sudo")
	}
}
