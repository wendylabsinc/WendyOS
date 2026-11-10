package oci

import (
	"encoding/json"
	"testing"

	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
)

func TestAppOOMScoreIsExplicitZero(t *testing.T) {
	for name, spec := range map[string]*Spec{
		"default": DefaultSpec("/rootfs", []string{"/bin/sh"}),
		"legacy":  {Process: &Process{Args: []string{"/bin/sh"}}},
	} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			var runtimeSpec runtimespec.Spec
			if err := json.Unmarshal(data, &runtimeSpec); err != nil {
				t.Fatal(err)
			}
			if runtimeSpec.Process.OOMScoreAdj == nil {
				t.Fatal("oomScoreAdj omitted; runtime would inherit the shim's OOM protection")
			}
			if got := *runtimeSpec.Process.OOMScoreAdj; got != 0 {
				t.Fatalf("oomScoreAdj = %d; want 0", got)
			}
		})
	}
}

// TestDefaultSeccompBlocksKernelAttackSurface is the regression test for
// WDY-1012: the baseline seccomp profile must deny the kernel-attack-surface
// syscalls a normal application container never needs (module loading, kexec),
// in addition to the existing ptrace/unshare/clone-newuser blocks. These are
// pure host-escape primitives; blocking them at seccomp is defense-in-depth on
// top of the capability gating that already withholds CAP_SYS_MODULE/BOOT.
func TestDefaultSeccompBlocksKernelAttackSurface(t *testing.T) {
	sc := defaultSeccomp()
	if sc == nil {
		t.Fatal("defaultSeccomp() returned nil; containers would have no seccomp filter")
	}
	blocked := map[string]bool{}
	for _, s := range sc.Syscalls {
		if s.Action == ActErrno && len(s.Args) == 0 {
			for _, n := range s.Names {
				blocked[n] = true
			}
		}
	}
	// Existing protections must remain.
	for _, name := range []string{"ptrace", "unshare"} {
		if !blocked[name] {
			t.Errorf("seccomp should still unconditionally block %q", name)
		}
	}
	// WDY-1012: dangerous, never-needed kernel-surface syscalls.
	for _, name := range []string{
		"kexec_load", "kexec_file_load",
		"init_module", "finit_module", "delete_module", "create_module",
	} {
		if !blocked[name] {
			t.Errorf("seccomp must block dangerous syscall %q (WDY-1012)", name)
		}
	}
}

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

func TestDropToMinimalCapabilities(t *testing.T) {
	spec := DefaultSpec("rootfs", []string{"sleep", "infinity"})
	DropToMinimalCapabilities(spec)
	caps := spec.Process.Capabilities
	if caps == nil {
		t.Fatal("Capabilities is nil after DropToMinimalCapabilities")
	}
	// Assert every capability class is fully empty — this is the primary invariant.
	// The dangerous-cap loop below is vacuously true when sets are empty; this check
	// catches regressions where a set is non-empty but happens to miss the short list.
	for _, set := range [][]string{caps.Bounding, caps.Effective, caps.Permitted, caps.Inheritable, caps.Ambient} {
		if len(set) != 0 {
			t.Errorf("expected empty capability set, got %v", set)
		}
	}
	dangerous := []string{"CAP_NET_RAW", "CAP_MKNOD", "CAP_SETUID", "CAP_SETPCAP"}
	for _, set := range [][]string{caps.Bounding, caps.Effective, caps.Permitted, caps.Inheritable, caps.Ambient} {
		for _, c := range set {
			for _, bad := range dangerous {
				if c == bad {
					t.Errorf("minimal caps must not include %s", c)
				}
			}
		}
	}
	// Verify NoNewPrivileges is preserved.
	if !spec.Process.NoNewPrivileges {
		t.Error("NoNewPrivileges must remain true after DropToMinimalCapabilities")
	}
}

func TestInjectHostsMount(t *testing.T) {
	spec := DefaultSpec("rootfs", []string{"/bin/sh"})
	InjectHostsMount(spec, "/run/wendy/hosts/com.example.app")
	var found bool
	for _, m := range spec.Mounts {
		if m.Destination == "/etc/hosts" {
			found = true
			if m.Source != "/run/wendy/hosts/com.example.app" {
				t.Errorf("Source = %q, want /run/wendy/hosts/com.example.app", m.Source)
			}
			if m.Type != "bind" {
				t.Errorf("Type = %q, want bind", m.Type)
			}
			hasRO := false
			for _, opt := range m.Options {
				if opt == "ro" {
					hasRO = true
				}
			}
			if !hasRO {
				t.Errorf("Options %v should contain 'ro'", m.Options)
			}
		}
	}
	if !found {
		t.Error("expected /etc/hosts bind-mount in spec")
	}
}
