package configpartition

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	for failure, want := range map[string]string{
		"invalid name": "invalid device name",
		"read fails":   "reading previous device name",
		"write fails":  "writing device name",
	} {
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
			stored := filepath.Join(root, "etc/wendyos/device-name")
			switch failure {
			case "read fails":
				name = "valid-name"
				// A directory at the destination fails even when CI runs as root.
				if err := os.MkdirAll(stored, 0o755); err != nil {
					t.Fatal(err)
				}
			case "write fails":
				name = "valid-name"
				// So does a dangling link: nothing to read back, nowhere to write.
				if err := os.MkdirAll(filepath.Dir(stored), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "missing/device-name"), stored); err != nil {
					t.Fatal(err)
				}
			}
			if err := persistDeviceName(zap.NewNop(), root, name); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("persistDeviceName = %v; want %q failure", err, want)
			}
			got, err := os.ReadFile(hostname)
			if err != nil || string(got) != "previous\n" {
				t.Fatalf("previous identity = %q, %v", got, err)
			}
		})
	}
}

func TestPersistDeviceNameRejectsWhenOverrideCannotBeRemoved(t *testing.T) {
	for _, previous := range []string{"old-device\n", ""} {
		t.Run(fmt.Sprintf("previous=%q", previous), func(t *testing.T) {
			root := t.TempDir()
			blocked := filepath.Join(root, "etc/wendy-agent/hostname/child")
			if err := os.MkdirAll(blocked, 0o755); err != nil {
				t.Fatal(err)
			}
			stored := filepath.Join(root, "etc/wendyos/device-name")
			if previous != "" {
				if err := os.MkdirAll(filepath.Dir(stored), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(stored, []byte(previous), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			core, logs := observer.New(zap.WarnLevel)
			if err := persistDeviceName(zap.New(core), root, "new-device"); err == nil {
				t.Fatal("reported success while an explicit hostname still overrides the device name")
			}
			if logs.FilterMessage("Could not clear explicit hostname; it will keep overriding the device name").Len() != 1 {
				t.Fatalf("missing warning: %v", logs.All())
			}
			// The rejected name must not stay on disk to take effect later.
			got, err := os.ReadFile(stored)
			if previous == "" {
				if !os.IsNotExist(err) {
					t.Fatalf("failed rename left a stored device name: %q, %v", got, err)
				}
			} else if err != nil || string(got) != previous {
				t.Fatalf("stored device name after failed rename = %q, %v; want %q", got, err, previous)
			}
		})
	}
}
