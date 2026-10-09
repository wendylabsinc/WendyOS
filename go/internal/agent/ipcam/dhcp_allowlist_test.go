package ipcam

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseDHCPAllowlist(t *testing.T) {
	got, err := parseDHCPAllowlist("# direct camera ports\neth1\n enp2s0 \neth1\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got["eth1"] || !got["enp2s0"] || got["eth0"] {
		t.Fatalf("unexpected camera DHCP interfaces: %v", got)
	}
	for _, bad := range []string{"eth0 eth1", "eth0/1", "eth0*", "1234567890123456"} {
		if _, err := parseDHCPAllowlist(bad); err == nil {
			t.Errorf("accepted invalid camera DHCP interface %q", bad)
		}
	}
}

func TestReadDHCPAllowlistRequiresRootOwnedRegularFile(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to create a root-owned fixture")
	}
	path := filepath.Join(t.TempDir(), "camera-dhcp-interfaces")
	if allowed, err := readDHCPAllowlist(path); err != nil || len(allowed) != 0 {
		t.Fatalf("missing file = %v, %v; want empty allowance", allowed, err)
	}
	if err := os.WriteFile(path, []byte("eth1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if allowed, err := readDHCPAllowlist(path); err != nil || !allowed["eth1"] || allowed["eth0"] {
		t.Fatalf("valid file = %v, %v", allowed, err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := readDHCPAllowlist(path); err == nil {
		t.Fatal("accepted an allowlist writable by other users")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other", path); err != nil {
		t.Fatal(err)
	}
	if _, err := readDHCPAllowlist(path); err == nil {
		t.Fatal("accepted a symlink allowlist")
	}
}
