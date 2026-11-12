package configpartition

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestPersistDeviceNameClearsExplicitHostname(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "missing"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			hostname := filepath.Join(root, "etc/wendy-agent/hostname")
			if err := os.MkdirAll(filepath.Dir(hostname), 0o755); err != nil {
				t.Fatal(err)
			}
			if kind != "missing" {
				target := hostname
				if kind == "symlink" {
					target = filepath.Join(root, "persistent-hostname")
				}
				if err := os.WriteFile(target, []byte("old-explicit-name\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					if err := os.Symlink(target, hostname); err != nil {
						t.Fatal(err)
					}
				}
			}
			for range 2 { // Reapplying first-boot configuration must remain safe.
				if err := persistDeviceName(zap.NewNop(), root, "new-device"); err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(filepath.Join(root, "etc/wendyos/device-name"))
				if err != nil || string(got) != "new-device\n" {
					t.Fatalf("device name = %q, %v", got, err)
				}
				if _, err := os.Lstat(hostname); !os.IsNotExist(err) {
					t.Fatalf("explicit override survives: %v", err)
				}
			}
		})
	}
}

func TestPersistDeviceNameFailureKeepsPreviousIdentity(t *testing.T) {
	for _, failure := range []string{"invalid name", "write fails"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			hostname := filepath.Join(root, "etc/wendy-agent/hostname")
			if err := os.MkdirAll(filepath.Dir(hostname), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(hostname, []byte("previous\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			name := "BAD NAME"
			if failure == "write fails" {
				name = "valid-name"
				// A directory at the destination fails even when CI runs as root.
				if err := os.MkdirAll(filepath.Join(root, "etc/wendyos/device-name"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := persistDeviceName(zap.NewNop(), root, name); err == nil {
				t.Fatal("expected failure")
			}
			got, err := os.ReadFile(hostname)
			if err != nil || string(got) != "previous\n" {
				t.Fatalf("previous identity = %q, %v", got, err)
			}
		})
	}
}

func TestPersistDeviceNameWarnsWhenOverrideCannotBeRemoved(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "etc/wendy-agent/hostname/child")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.WarnLevel)
	if err := persistDeviceName(zap.New(core), root, "new-device"); err != nil {
		t.Fatal(err)
	}
	if logs.FilterMessage("Could not clear explicit hostname; it will keep overriding the device name").Len() != 1 {
		t.Fatalf("missing warning: %v", logs.All())
	}
}
