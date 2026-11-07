//go:build !windows

package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWritePreservingOwnerChownsTheTempFileToTheExistingOwner(t *testing.T) {
	var gotPath string
	var gotUID, gotGID int
	called := false
	stubOwner(t, 0, func(name string, uid, gid int) error {
		called = true
		gotPath, gotUID, gotGID = name, uid, gid
		return nil
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantUID, wantGID := os.Getuid(), os.Getgid() // the file's real owner

	if err := WritePreservingOwner(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WritePreservingOwner: %v", err)
	}
	if !called {
		t.Fatal("WritePreservingOwner never chowned although geteuid was stubbed to 0")
	}
	if gotPath == path || gotPath == "" || filepath.Dir(gotPath) != dir {
		t.Errorf("chown target = %q, want the temp file in %q, not the target %q", gotPath, dir, path)
	}
	if gotUID != wantUID || gotGID != wantGID {
		t.Errorf("chown(uid=%d,gid=%d), want the existing file's owner (%d,%d)", gotUID, gotGID, wantUID, wantGID)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Errorf("contents = %q, want new", data)
	}
}

func TestWritePreservingOwnerChownsToTheDirOwnerForANewFile(t *testing.T) {
	var gotUID, gotGID int
	called := false
	stubOwner(t, 0, func(_ string, uid, gid int) error {
		called = true
		gotUID, gotGID = uid, gid
		return nil
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")    // does not exist yet
	wantUID, wantGID := os.Getuid(), os.Getgid() // the directory's real owner

	if err := WritePreservingOwner(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WritePreservingOwner: %v", err)
	}
	if !called {
		t.Fatal("WritePreservingOwner never chowned a new file's temp file")
	}
	if gotUID != wantUID || gotGID != wantGID {
		t.Errorf("chown(uid=%d,gid=%d), want the directory's owner (%d,%d)", gotUID, gotGID, wantUID, wantGID)
	}
}
