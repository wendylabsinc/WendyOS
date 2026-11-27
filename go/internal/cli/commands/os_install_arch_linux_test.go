package commands

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Exercise the real AUR package function with GNU install, without downloading
// release artifacts or installing a package on the test host.
func TestArchPackageJetsonPermissions(t *testing.T) {
	packaging, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "packaging"))
	if err != nil {
		t.Fatal(err)
	}
	rule, err := os.ReadFile(filepath.Join(packaging, "linux", "udev", "70-wendy-jetson.rules"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		arch, binaryDir string
	}{
		{"x86_64", "wendy-cli-linux-amd64"},
		{"aarch64", "wendy-cli-linux-arm64"},
	} {
		t.Run(tc.arch, func(t *testing.T) {
			root := t.TempDir()
			srcdir, pkgdir := filepath.Join(root, "src"), filepath.Join(root, "pkg")
			binaryDir := filepath.Join(srcdir, tc.binaryDir)
			if err := os.MkdirAll(binaryDir, 0o755); err != nil {
				t.Fatal(err)
			}
			binary := []byte("wendy binary fixture for " + tc.arch + "\n")
			if err := os.WriteFile(filepath.Join(binaryDir, "wendy"), binary, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-c", "set -euo pipefail\nsource \"$1\"\npackage", "package-test", filepath.Join(packaging, "arch", "wendy", "PKGBUILD"))
			cmd.Env = append(os.Environ(), "CARCH="+tc.arch, "srcdir="+srcdir, "pkgdir="+pkgdir)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("PKGBUILD package(): %v\n%s", err, out)
			}
			for _, file := range []struct {
				path string
				data []byte
				mode os.FileMode
			}{
				{"usr/bin/wendy", binary, 0o755},
				{"usr/lib/udev/rules.d/70-wendy-jetson.rules", rule, 0o644},
				{"usr/lib/sysusers.d/wendy.conf", []byte("g plugdev -\n"), 0o644},
			} {
				path := filepath.Join(pkgdir, file.path)
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(file.data) {
					t.Errorf("%s contents = %q; want %q", file.path, got, file.data)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if !info.Mode().IsRegular() || info.Mode().Perm() != file.mode {
					t.Errorf("%s mode = %s; want regular file with mode %s", file.path, info.Mode(), file.mode)
				}
			}
		})
	}
}
