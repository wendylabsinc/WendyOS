//go:build linux

package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxedVersionRefusesWithoutSystemdRun(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	script := filepath.Join(dir, "agent")
	// A shell builtin, so the marker appears even with an empty PATH.
	os.WriteFile(script, []byte("#!/bin/sh\n: > "+marker+"\necho 1.0\n"), 0o755)
	t.Setenv("PATH", t.TempDir())

	if _, err := SandboxedVersion(context.Background(), script); err == nil {
		t.Fatal("want a refusal when the sandbox is not available")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the binary must never run outside the sandbox")
	}
}

func TestSandboxedVersionLocksTheBinaryDown(t *testing.T) {
	bin := t.TempDir()
	args := filepath.Join(t.TempDir(), "args")
	fake := "#!/bin/sh\nfor a in \"$@\"; do echo \"$a\" >> " + args + "; done\necho 2026.11.02-101010\n"
	os.WriteFile(filepath.Join(bin, "systemd-run"), []byte(fake), 0o755)
	t.Setenv("PATH", bin)

	out, err := SandboxedVersion(context.Background(), "/data/wendy-agent/agent/inbox/abc")
	if err != nil || strings.TrimSpace(out) != "2026.11.02-101010" {
		t.Fatalf("SandboxedVersion = %q, %v", out, err)
	}
	raw, _ := os.ReadFile(args)
	got := string(raw)
	for _, want := range []string{
		"CapabilityBoundingSet=", "NoNewPrivileges=yes", "PrivateNetwork=yes", "SystemCallFilter=~@network-io",
		"PrivateDevices=yes", "ProtectSystem=strict", "RuntimeMaxSec=5",
		"--\n/data/wendy-agent/agent/inbox/abc\n--version",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("systemd-run is missing %q; args:\n%s", want, got)
		}
	}
}

// sandboxRun runs argv under the sandbox's properties through the user's own
// systemd manager, which checks the properties as the system one does.
func sandboxRun(t *testing.T, argv ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("no systemd-run")
	}
	if exec.Command("systemctl", "--user", "is-system-running").Run() != nil {
		if out, _ := exec.Command("systemctl", "--user", "is-system-running").Output(); len(out) == 0 {
			t.Skip("no systemd user manager")
		}
	}
	args := []string{"--user", "--quiet", "--wait", "--pipe", "--collect", "--service-type=exec"}
	for _, p := range sandboxProperties {
		args = append(args, "-p", p)
	}
	out, err := exec.Command("systemd-run", append(append(args, "--"), argv...)...).CombinedOutput()
	return string(out), err
}

func TestSystemdAcceptsTheSandbox(t *testing.T) {
	// A fake systemd-run hid a property the real one refuses.
	if out, err := sandboxRun(t, "/bin/echo", "ran"); err != nil || strings.TrimSpace(out) != "ran" {
		t.Fatalf("systemd refused the sandbox: %v\n%s", err, out)
	}
}

func TestTheSandboxAllowsNoSockets(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	out, _ := sandboxRun(t, py, "-c", "import socket\ntry:\n socket.socket(socket.AF_UNIX)\n print('socket')\nexcept OSError:\n print('refused')")
	if strings.TrimSpace(out) != "refused" {
		t.Fatalf("a socket was created in the sandbox: %q", out)
	}
}
