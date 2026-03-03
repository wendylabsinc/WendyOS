package oci

import (
	"encoding/json"
	"testing"
)

func TestDefaultSpec(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})

	if spec.OCIVersion != "1.0.2" {
		t.Errorf("OCIVersion = %q, want %q", spec.OCIVersion, "1.0.2")
	}

	if spec.Process == nil {
		t.Fatal("Process is nil")
	}
	if len(spec.Process.Args) != 1 || spec.Process.Args[0] != "/bin/sh" {
		t.Errorf("Process.Args = %v, want [/bin/sh]", spec.Process.Args)
	}
	if spec.Process.Cwd != "/" {
		t.Errorf("Process.Cwd = %q, want %q", spec.Process.Cwd, "/")
	}
	if spec.Process.User.UID != 0 || spec.Process.User.GID != 0 {
		t.Errorf("Process.User = {UID:%d, GID:%d}, want {UID:0, GID:0}",
			spec.Process.User.UID, spec.Process.User.GID)
	}
	if !spec.Process.NoNewPrivileges {
		t.Error("Process.NoNewPrivileges = false, want true")
	}

	if spec.Root == nil {
		t.Fatal("Root is nil")
	}
	if spec.Root.Path != "/rootfs" {
		t.Errorf("Root.Path = %q, want %q", spec.Root.Path, "/rootfs")
	}
	if spec.Root.Readonly {
		t.Error("Root.Readonly = true, want false")
	}

	if spec.Hostname != "wendy" {
		t.Errorf("Hostname = %q, want %q", spec.Hostname, "wendy")
	}

	// Should have capabilities set.
	if spec.Process.Capabilities == nil {
		t.Fatal("Process.Capabilities is nil")
	}
	if len(spec.Process.Capabilities.Bounding) == 0 {
		t.Error("Process.Capabilities.Bounding is empty")
	}

	// Should have environment variables.
	if len(spec.Process.Env) < 2 {
		t.Errorf("Process.Env has %d entries, want at least 2", len(spec.Process.Env))
	}
}

func TestSpecJSON(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh", "-c", "echo hello"})

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var decoded Spec
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if decoded.OCIVersion != spec.OCIVersion {
		t.Errorf("round-trip OCIVersion = %q, want %q", decoded.OCIVersion, spec.OCIVersion)
	}
	if decoded.Root.Path != spec.Root.Path {
		t.Errorf("round-trip Root.Path = %q, want %q", decoded.Root.Path, spec.Root.Path)
	}
	if len(decoded.Process.Args) != 3 {
		t.Errorf("round-trip Process.Args length = %d, want 3", len(decoded.Process.Args))
	}
	if decoded.Hostname != "wendy" {
		t.Errorf("round-trip Hostname = %q, want %q", decoded.Hostname, "wendy")
	}
}

func TestSpecMounts(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})

	if len(spec.Mounts) == 0 {
		t.Fatal("Mounts is empty, want default mounts")
	}

	// Check that /proc mount exists.
	foundProc := false
	foundDev := false
	foundSys := false
	for _, m := range spec.Mounts {
		switch m.Destination {
		case "/proc":
			foundProc = true
			if m.Type != "proc" {
				t.Errorf("/proc mount type = %q, want %q", m.Type, "proc")
			}
		case "/dev":
			foundDev = true
			if m.Type != "tmpfs" {
				t.Errorf("/dev mount type = %q, want %q", m.Type, "tmpfs")
			}
		case "/sys":
			foundSys = true
			if m.Type != "sysfs" {
				t.Errorf("/sys mount type = %q, want %q", m.Type, "sysfs")
			}
		}
	}

	if !foundProc {
		t.Error("missing /proc mount")
	}
	if !foundDev {
		t.Error("missing /dev mount")
	}
	if !foundSys {
		t.Error("missing /sys mount")
	}
}

func TestSpecNamespaces(t *testing.T) {
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})

	if spec.Linux == nil {
		t.Fatal("Linux is nil")
	}

	expected := map[string]bool{
		"pid":     false,
		"ipc":     false,
		"uts":     false,
		"mount":   false,
		"network": false,
	}

	for _, ns := range spec.Linux.Namespaces {
		if _, ok := expected[ns.Type]; ok {
			expected[ns.Type] = true
		}
	}

	for nsType, found := range expected {
		if !found {
			t.Errorf("missing namespace: %s", nsType)
		}
	}
}
