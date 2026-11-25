package oci

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestWorldSocketMountRequiresExactEntitlementAndSocket(t *testing.T) {
	directory := listenInTempDir(t, "world.sock")
	if err := ValidateWorldSocketDirectory(directory); err != nil {
		t.Fatal(err)
	}
	spec := DefaultSpec("/fixture/root", nil)
	ApplyHostSockets(spec, &appconfig.AppConfig{}, ApplyOptions{WorldSocketDir: directory})
	for _, mount := range spec.Mounts {
		if mount.Destination == "/run/wendy/world" {
			t.Fatal("unentitled mount")
		}
	}
	ApplyHostSockets(spec, &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementWorld}}}, ApplyOptions{WorldSocketDir: directory})
	found := false
	for _, mount := range spec.Mounts {
		if mount.Destination == "/run/wendy/world" {
			found = true
			if mount.Source != directory {
				t.Fatal("wrong source")
			}
		}
	}
	if !found {
		t.Fatal("no World mount")
	}
}

func TestWorldSocketRejectsRegularFileAndDirectoryAlias(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "world.sock"), []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}
	if ValidateWorldSocketDirectory(directory) == nil {
		t.Fatal("regular file admitted")
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	if ValidateWorldSocketDirectory(alias) == nil {
		t.Fatal("symlink admitted")
	}
}
