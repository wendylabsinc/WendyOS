package agentmodule

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func commissionedDirectory(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func commissionedFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "world.json")
	if err := os.WriteFile(path, []byte(`{"commissioned":"fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommissionedFileAcceptsProtectedFileAndBoundsReads(t *testing.T) {
	path := commissionedFile(t, commissionedDirectory(t))
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadCommissionedFile(path, 4096)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("protected commissioned file rejected", err)
	}
	for _, limit := range []int{-1, 0, 1, 257 << 10} {
		if _, err := ReadCommissionedFile(path, limit); err == nil {
			t.Fatal("invalid/insufficient read budget accepted", limit)
		}
	}
}

func TestCommissionedFileRefusesSymlinksAndWritableAncestors(t *testing.T) {
	for _, attack := range []string{"final-symlink", "ancestor-symlink", "writable-ancestor", "writable-file", "directory", "fifo"} {
		t.Run(attack, func(t *testing.T) {
			dir := commissionedDirectory(t)
			child := filepath.Join(dir, "private")
			if err := os.Mkdir(child, 0700); err != nil {
				t.Fatal(err)
			}
			path := commissionedFile(t, child)
			switch attack {
			case "final-symlink":
				link := filepath.Join(child, "alias.json")
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			case "ancestor-symlink":
				link := filepath.Join(dir, "alias")
				if err := os.Symlink(child, link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, "world.json")
			case "writable-ancestor":
				if err := os.Chmod(child, 0777); err != nil {
					t.Fatal(err)
				}
			case "writable-file":
				if err := os.Chmod(path, 0660); err != nil {
					t.Fatal(err)
				}
			case "directory":
				path = child
			case "fifo":
				path = filepath.Join(child, "fifo")
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ReadCommissionedFile(path, 4096); err == nil {
				t.Fatal("untrusted file path accepted", attack)
			}
		})
	}
}

func TestCommissionedFileStickyExceptionRequiresRootOwner(t *testing.T) {
	dir := commissionedDirectory(t)
	sticky := filepath.Join(dir, "sticky")
	if err := os.Mkdir(sticky, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	path := commissionedFile(t, sticky)
	_, err := ReadCommissionedFile(path, 4096)
	if os.Geteuid() == 0 {
		if err != nil {
			t.Fatal("root sticky directory rejected", err)
		}
	} else if err == nil {
		t.Fatal("non-root sticky writable ancestor accepted")
	}
}

func TestCommissionedFileRefusesForeignOwnedAncestor(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("ownership transition requires isolated root test process")
	}
	dir := commissionedDirectory(t)
	child := filepath.Join(dir, "foreign")
	if err := os.Mkdir(child, 0755); err != nil {
		t.Fatal(err)
	}
	path := commissionedFile(t, child)
	if err := os.Chown(child, 12345, 12345); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCommissionedFile(path, 4096); err == nil {
		t.Fatal("foreign ancestor accepted")
	}
}

func TestAbsentCommissionedFileAndParentLeaveWorldDisabled(t *testing.T) {
	dir := commissionedDirectory(t)
	for _, path := range []string{filepath.Join(dir, "world.json"), filepath.Join(dir, "missing", "world.json")} {
		if _, err := ReadCommissionedFile(path, 4096); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("missing optional configuration lost not-exist classification", err)
		}
		manager, err := Load(context.Background(), path, nil, nil)
		if err != nil || manager != nil {
			t.Fatal("absent configuration did not leave World disabled", err)
		}
	}
}
