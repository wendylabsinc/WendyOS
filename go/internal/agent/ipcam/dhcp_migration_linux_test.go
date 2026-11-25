//go:build linux

package ipcam

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeLegacyCameras(t *testing.T, path string, cameras []Camera) {
	t.Helper()
	data, err := json.Marshal(cameras)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Return an unloaded store so migration must read the persisted credentials.
func writeLegacyCredentials(t *testing.T, path string, macs ...string) *CredentialStore {
	t.Helper()
	credentials := make(map[string]Credential)
	for _, mac := range macs {
		credentials[mac] = Credential{Username: "admin"}
	}
	data, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return NewCredentialStore(path)
}

func TestDHCPLinuxMigrationPreservesOperatorDisableAcrossRestarts(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned configuration")
	}
	h := newManagerHarness(t, []Interface{cabledEth("eth0"), cabledEth("eth1"), cabledEth("eth2")})
	path := filepath.Join(t.TempDir(), "config", "camera-dhcp-interfaces")
	credentials := writeLegacyCredentials(t, filepath.Join(t.TempDir(), "credentials.json"), "02:00:00:00:00:01", "02:00:00:00:00:02")
	// These records predate both the served flag and the opt-in policy. An
	// intermediate agent has left them offline for months without creating a
	// permission file. Migration must not depend on current liveness or age.
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeLegacyCameras(t, h.reg.path, []Camera{
		{MAC: "02:00:00:00:00:01", ID: 200, Link: "eth0", Address: "10.98.7.50", FirstSeen: old, LastSeen: old},
		{MAC: "02:00:00:00:00:02", ID: 201, Link: "eth1", Address: "10.98.8.149", FirstSeen: old, LastSeen: old},
		// This old ordinary client was served by mistake and has no stored login.
		{MAC: "02:00:00:00:00:03", ID: 202, Link: "eth2", Address: "10.98.9.50", Model: "OfficePrinter", DHCPServed: true},
	})
	if err := h.reg.Load(); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.LoadDHCPAllowlist(path); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.MigrateLegacyDHCPAllowlist(credentials); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "eth0\neth1\n" {
		t.Fatalf("migrated permissions = %q, %v", data, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("permission file must be private: %v, %v", info, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.mgr.scanOnce(ctx)
	waitFor(t, "migrated ports to be watched", func() bool { return len(h.watchedLinks()) == 3 })
	if len(h.servedLinks()) != 0 {
		t.Fatal("migration bypassed the competing-server observation window")
	}
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	waitFor(t, "migrated ports to serve", func() bool { return len(h.servedLinks()) == 2 })
	if h.mgr.dhcpAllowed("eth2") || h.mgr.isClaimed("eth2") || len(h.addedAddresses()) != 2 {
		t.Fatal("unconfirmed client enabled its port")
	}
	if seg, ok := h.mgr.claimedSegment("eth0"); !ok || seg.CIDR() != "10.98.7.1/24" {
		t.Fatalf("legacy segment not restored: %+v, %v", seg, ok)
	}
	// This is the operator RPC's implementation. Preserve the other migrated
	// port, then disable the last one: the empty file must still block migration.
	if _, err := h.mgr.SetDHCPEnabled("eth0", false); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "eth1\n" {
		t.Fatalf("single-port disable = %q, %v", data, err)
	}
	if err := h.mgr.MigrateLegacyDHCPAllowlist(credentials); err != nil {
		t.Fatal(err)
	}
	if h.mgr.dhcpAllowed("eth0") || !h.mgr.dhcpAllowed("eth1") {
		t.Fatal("migration overrode a single-port disable")
	}
	if _, err := h.mgr.SetDHCPEnabled("eth1", false); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "\n" {
		t.Fatalf("last-port disable = %q, %v", data, err)
	}
	cancel()
	for restart := 0; restart < 2; restart++ {
		next := newManagerHarness(t, []Interface{cabledEth("eth0"), cabledEth("eth1"), cabledEth("eth2")})
		next.reg = NewRegistry(h.reg.path)
		if err := next.reg.Load(); err != nil {
			t.Fatal(err)
		}
		next.mgr.reg = next.reg
		if err := next.mgr.LoadDHCPAllowlist(path); err != nil {
			t.Fatal(err)
		}
		if err := next.mgr.MigrateLegacyDHCPAllowlist(credentials); err != nil {
			t.Fatal(err)
		}
		ctx, stop := context.WithCancel(context.Background())
		t.Cleanup(stop)
		next.mgr.scanOnce(ctx)
		waitFor(t, "disabled ports to be watched", func() bool { return len(next.watchedLinks()) == 3 })
		next.feed("eth0", &Packet{Type: Discover, XID: 1})
		next.advance(unansweredWindow)
		next.mgr.scanOnce(ctx)
		if next.mgr.dhcpAllowed("eth0") || next.mgr.dhcpAllowed("eth1") || len(next.servedLinks()) != 0 || len(next.addedAddresses()) != 0 {
			t.Fatal("historical records reactivated operator-disabled DHCP after restart")
		}
		stop()
	}
}

func TestDHCPLinuxMigrationExistingConfigurationWins(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned configuration")
	}
	for _, contents := range []string{"", "\n", "# disabled by operator\n", "eth2\n"} {
		t.Run(fmt.Sprintf("configuration_%q", contents), func(t *testing.T) {
			h := newManagerHarness(t, nil)
			path := filepath.Join(t.TempDir(), "camera-dhcp-interfaces")
			credentials := writeLegacyCredentials(t, filepath.Join(t.TempDir(), "credentials.json"), "02:00:00:00:00:01")
			writeLegacyCameras(t, h.reg.path, []Camera{{MAC: "02:00:00:00:00:01", Link: "eth0", Address: "10.98.0.50"}})
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := h.mgr.LoadDHCPAllowlist(path); err != nil {
				t.Fatal(err)
			}
			// Existing settings win even when credentials cannot be parsed.
			if err := os.WriteFile(credentials.path, []byte("invalid credentials"), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.mgr.MigrateLegacyDHCPAllowlist(credentials); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != contents || h.mgr.dhcpAllowed("eth0") {
				t.Fatalf("existing configuration overwritten: %q, %v", data, err)
			}
			after, err := os.Stat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("migration replaced an existing configuration file")
			}
		})
	}
}

func TestDHCPLinuxMigrationEmptyResultCompletesOnce(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned configuration")
	}
	h := newManagerHarness(t, nil)
	path := filepath.Join(t.TempDir(), "camera-dhcp-interfaces")
	// A historical lease without a credential file must finish with no permission.
	writeLegacyCameras(t, h.reg.path, []Camera{{MAC: "02:00:00:00:00:01", Link: "eth0", Address: "10.98.0.50"}})
	credentials := NewCredentialStore(filepath.Join(t.TempDir(), "credentials.json"))
	if err := h.mgr.LoadDHCPAllowlist(path); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.MigrateLegacyDHCPAllowlist(credentials); err != nil {
		t.Fatal(err)
	}
	credentials = writeLegacyCredentials(t, credentials.path, "02:00:00:00:00:01")
	if err := h.mgr.LoadDHCPAllowlist(path); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.MigrateLegacyDHCPAllowlist(credentials); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "\n" || h.mgr.dhcpAllowed("eth0") {
		t.Fatalf("later record re-ran migration: %q, %v", data, err)
	}
}

func TestDHCPLinuxMigrationFailuresCanRetry(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned configuration")
	}
	h := newManagerHarness(t, nil)
	path := filepath.Join(t.TempDir(), "camera-dhcp-interfaces")
	credentials := writeLegacyCredentials(t, filepath.Join(t.TempDir(), "credentials.json"), "02:00:00:00:00:01")
	if err := h.mgr.LoadDHCPAllowlist(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.reg.path, []byte("invalid registry"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.MigrateLegacyDHCPAllowlist(credentials); err == nil {
		t.Fatal("completed migration without a readable registry")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("failed migration created configuration: %v", err)
	}
	// Oversized legacy configuration fails before installing a partial file.
	var cameras []Camera
	var macs []string
	for i := 0; i < 300; i++ {
		macs = append(macs, fmt.Sprint(i))
		cameras = append(cameras, Camera{MAC: fmt.Sprint(i), Link: fmt.Sprintf("camera-port%04d", i), Address: "10.98.0.50"})
	}
	writeLegacyCameras(t, h.reg.path, cameras)
	credentials = writeLegacyCredentials(t, credentials.path, macs...)
	if err := h.mgr.MigrateLegacyDHCPAllowlist(credentials); err == nil {
		t.Fatal("completed migration with an oversized permission file")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("failed write installed configuration: %v", err)
	}
	writeLegacyCameras(t, h.reg.path, []Camera{{MAC: "02:00:00:00:00:01", Link: "eth0", Address: "10.98.0.50"}})
	credentials = writeLegacyCredentials(t, credentials.path, "02:00:00:00:00:01")
	if err := h.mgr.MigrateLegacyDHCPAllowlist(credentials); err != nil {
		t.Fatal(err)
	}
	if !h.mgr.dhcpAllowed("eth0") {
		t.Fatal("retry did not migrate legacy port")
	}
}

func TestDHCPLinuxMigrationCreateDoesNotReplaceConcurrentConfiguration(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned configuration")
	}
	path := filepath.Join(t.TempDir(), "camera-dhcp-interfaces")
	// Simulate the operator creating a disable after the migration's missing-file
	// check. Atomic installation must preserve that file, including its inode.
	if err := writeDHCPAllowlist(path, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := createDHCPAllowlist(path, map[string]bool{"eth0": true}); !os.IsExist(err) {
		t.Fatalf("expected existing-file conflict: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("migration replaced operator configuration")
	}
	allowed, err := readDHCPAllowlist(path)
	if err != nil || !reflect.DeepEqual(allowed, map[string]bool{}) {
		t.Fatalf("operator disable lost: %v, %v", allowed, err)
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 1 || strings.HasPrefix(files[0].Name(), ".camera-dhcp-") {
		t.Fatalf("temporary migration file leaked: %v, %v", files, err)
	}
}

func TestDHCPLinuxMigrationCredentialErrorsCanRetry(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned configuration")
	}
	for _, failure := range []string{"malformed", "unreadable", "unavailable"} {
		t.Run(failure, func(t *testing.T) {
			h := newManagerHarness(t, nil)
			path := filepath.Join(t.TempDir(), "camera-dhcp-interfaces")
			writeLegacyCameras(t, h.reg.path, []Camera{{MAC: "02:00:00:00:00:01", Link: "eth0", Address: "10.98.0.50"}})
			credentialsPath := filepath.Join(t.TempDir(), "credentials.json")
			credentials := NewCredentialStore(credentialsPath)
			switch failure {
			case "malformed":
				if err := os.WriteFile(credentials.path, []byte("invalid credentials"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if err := os.Mkdir(credentials.path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "unavailable":
				credentials = nil
			}
			if err := h.mgr.LoadDHCPAllowlist(path); err != nil {
				t.Fatal(err)
			}
			if err := h.mgr.MigrateLegacyDHCPAllowlist(credentials); err == nil {
				t.Fatal("completed migration without readable credentials")
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("credential failure completed migration: %v", err)
			}
			if h.mgr.dhcpAllowed("eth0") {
				t.Fatal("credential failure enabled DHCP")
			}
			if credentials != nil {
				if err := os.Remove(credentials.path); err != nil {
					t.Fatal(err)
				}
			}
			credentials = writeLegacyCredentials(t, credentialsPath, "02:00:00:00:00:01")
			if err := h.mgr.MigrateLegacyDHCPAllowlist(credentials); err != nil {
				t.Fatal(err)
			}
			if !h.mgr.dhcpAllowed("eth0") {
				t.Fatal("retry did not migrate credential-matched port")
			}
		})
	}
}
