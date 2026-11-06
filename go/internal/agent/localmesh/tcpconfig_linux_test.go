//go:build linux

package localmesh

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestTCPConfigRequiresOwnedProtectedFile(t *testing.T) {
	self, _ := testDevices(t)
	for _, scenario := range []string{"file-writable", "directory-writable", "symlink", "foreign-owner", "fifo"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "local-mesh.json")
			if err := os.WriteFile(path, []byte(`{"nan":true}`), 0600); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "file-writable":
				if err := os.Chmod(path, 0666); err != nil {
					t.Fatal(err)
				}
			case "directory-writable":
				if err := os.Chmod(dir, 0777); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".source"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".source", path); err != nil {
					t.Fatal(err)
				}
			case "foreign-owner":
				if os.Geteuid() != 0 {
					t.Skip("chown needs root in a disposable container")
				}
				if err := os.Chown(path, 65534, 65534); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := LoadTCPConfig(path, self); err == nil {
				t.Fatal("unsafe configuration was accepted")
			}
			if _, err := ReadTCPConfigFile(path); err == nil {
				t.Fatal("unsafe watch snapshot was accepted")
			}
		})
	}
}
