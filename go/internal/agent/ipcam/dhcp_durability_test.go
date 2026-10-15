package ipcam

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDHCPConfigurationSyncsCreatedDirectoriesAndRename(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned configuration")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "parent", "config")
	path := filepath.Join(dir, "camera-dhcp-interfaces")
	var synced []string
	err := writeDHCPAllowlistWithSync(path, map[string]bool{"eth1": true}, func(directory string) error {
		synced = append(synced, directory)
		if directory == dir {
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "eth1\n" {
				t.Fatalf("directory synced before rename: %q, %v", data, err)
			}
		}
		return syncDHCPDirectory(directory)
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{base, filepath.Join(base, "parent"), dir}; len(synced) < len(want) || !reflect.DeepEqual(synced[len(synced)-len(want):], want) {
		t.Fatalf("directory syncs = %v, want %v", synced, want)
	}
	syncErr := errors.New("directory sync failed")
	err = writeDHCPAllowlistWithSync(path, map[string]bool{}, func(directory string) error {
		if directory == dir {
			return syncErr
		}
		return syncDHCPDirectory(directory)
	})
	if !errors.Is(err, syncErr) {
		t.Fatalf("durability failure reported success: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "\n" {
		t.Fatalf("expected renamed file despite sync failure: %q, %v", data, err)
	}
}

func TestDHCPDisableAfterRenameSyncFailureStillStopsServing(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	configureDHCPControl(t, h, map[string]bool{"eth0": true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claimEth0(t, h, ctx)
	write := h.mgr.writeAllowlist
	h.mgr.writeAllowlist = func(path string, allowed map[string]bool) error {
		if err := write(path, allowed); err != nil {
			return err
		}
		return errors.New("directory sync failed after rename")
	}
	if _, err := h.mgr.SetDHCPEnabled("eth0", false); err == nil {
		t.Fatal("durability failure reported success")
	}
	if h.mgr.isClaimed("eth0") || h.mgr.dhcpAllowed("eth0") {
		t.Fatal("committed disable left DHCP enabled")
	}
}

func TestDHCPConfigurationRetriesUnsyncedDirectoryCreation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned configuration")
	}
	base := t.TempDir()
	path := filepath.Join(base, "parent", "config", "camera-dhcp-interfaces")
	failure := errors.New("parent sync failed")
	if err := writeDHCPAllowlistWithSync(path, map[string]bool{"eth1": true}, func(dir string) error {
		if dir == base {
			return failure
		}
		return syncDHCPDirectory(dir)
	}); !errors.Is(err, failure) {
		t.Fatalf("directory creation failure: %v", err)
	}
	// The directories now exist, but that does not establish durability.
	retriedParent := false
	if err := writeDHCPAllowlistWithSync(path, map[string]bool{"eth1": true}, func(dir string) error {
		retriedParent = retriedParent || dir == base
		return syncDHCPDirectory(dir)
	}); err != nil {
		t.Fatal(err)
	}
	if !retriedParent {
		t.Fatal("retry acknowledged success without persisting the created parent")
	}
}
