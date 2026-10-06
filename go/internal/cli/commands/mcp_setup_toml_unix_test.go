//go:build unix

package commands

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// writeFileAtomic replaces the file with a new inode; it must hand the new
// file to the old one's owner, or a root run leaves the user a config file
// their AI tool can no longer read.
func TestWriteFileAtomic_KeepsOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	type call struct{ uid, gid int }
	var calls []call
	oldChown := chownFile
	chownFile = func(f *os.File, uid, gid int) error {
		calls = append(calls, call{uid, gid})
		return errors.New("operation not permitted") // best effort: must not fail the write
	}
	t.Cleanup(func() { chownFile = oldChown })

	if err := writeFileAtomic(path, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if want := (call{int(st.Uid), int(st.Gid)}); len(calls) != 1 || calls[0] != want {
		t.Fatalf("chown calls = %v, want [%v]", calls, want)
	}
	if got, _ := os.ReadFile(path); string(got) != "new\n" {
		t.Fatalf("content = %q", got)
	}

	// A new file takes its directory's owner, so `sudo wendy mcp setup`
	// does not create root-owned files in the user's home.
	calls = nil
	if err := writeFileAtomic(filepath.Join(filepath.Dir(path), "new.toml"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var dir syscall.Stat_t
	if err := syscall.Stat(filepath.Dir(path), &dir); err != nil {
		t.Fatal(err)
	}
	if want := (call{int(dir.Uid), int(dir.Gid)}); len(calls) != 1 || calls[0] != want {
		t.Fatalf("chown calls for a new file = %v, want [%v]", calls, want)
	}
}

type chownCall struct {
	path     string
	uid, gid int
}

// recordChownPath replaces chownPath with a recorder for the test.
func recordChownPath(t *testing.T) *[]chownCall {
	t.Helper()
	var calls []chownCall
	old := chownPath
	chownPath = func(path string, uid, gid int) error {
		calls = append(calls, chownCall{path, uid, gid})
		return nil
	}
	t.Cleanup(func() { chownPath = old })
	return &calls
}

func ownerOf(t *testing.T, path string) (int, int) {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return int(st.Uid), int(st.Gid)
}

// Directories setup creates take their parent's owner, top down.
func TestMkdirAllLikeParent(t *testing.T) {
	calls := recordChownPath(t)
	root := t.TempDir()
	uid, gid := ownerOf(t, root)
	dir := filepath.Join(root, "a", "b", "c")
	if err := mkdirAllLikeParent(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("%s not created (err=%v)", dir, err)
	}
	want := []chownCall{
		{filepath.Join(root, "a"), uid, gid},
		{filepath.Join(root, "a", "b"), uid, gid},
		{dir, uid, gid},
	}
	if len(*calls) != len(want) {
		t.Fatalf("chown calls = %v, want %v", *calls, want)
	}
	for i := range want {
		if (*calls)[i] != want[i] {
			t.Fatalf("chown calls = %v, want %v", *calls, want)
		}
	}
	*calls = nil
	if err := mkdirAllLikeParent(dir, 0o755); err != nil || len(*calls) != 0 {
		t.Fatalf("existing dir: err = %v, chown calls = %v", err, *calls)
	}
}

// A JSON config setup creates (and its directory) takes the owner of the
// directory it lands in; an existing one is written in place and keeps its own.
func TestAddMCPToJSONConfig_NewFileTakesDirOwner(t *testing.T) {
	calls := recordChownPath(t)
	root := t.TempDir()
	uid, gid := ownerOf(t, root)
	path := filepath.Join(root, ".cursor", "mcp.json")
	entry := map[string]any{"type": "stdio", "command": "/w", "args": []string{"mcp", "serve"}}
	if err := addMCPToJSONConfig(path, "mcpServers", "wendy", entry); err != nil {
		t.Fatal(err)
	}
	want := []chownCall{{filepath.Dir(path), uid, gid}, {path, uid, gid}}
	if len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
		t.Fatalf("chown calls = %v, want %v", *calls, want)
	}
	*calls = nil
	if err := addMCPToJSONConfig(path, "mcpServers", "wendy", entry); err != nil || len(*calls) != 0 {
		t.Fatalf("existing file: err = %v, chown calls = %v", err, *calls)
	}
}

// Same for a Codex config created from scratch: its new directory by path,
// the file itself through the temp file before the rename.
func TestAddMCPToTOMLConfig_NewFileTakesDirOwner(t *testing.T) {
	calls := recordChownPath(t)
	var fileCalls []chownCall
	oldChown := chownFile
	chownFile = func(f *os.File, uid, gid int) error {
		fileCalls = append(fileCalls, chownCall{"", uid, gid})
		return nil
	}
	t.Cleanup(func() { chownFile = oldChown })
	root := t.TempDir()
	uid, gid := ownerOf(t, root)
	path := filepath.Join(root, ".codex", "config.toml")
	if err := addMCPToTOMLConfig(path, "mcp_servers", "wendy", testWendyBin, testWendyArgs); err != nil {
		t.Fatal(err)
	}
	if want := (chownCall{filepath.Dir(path), uid, gid}); len(*calls) != 1 || (*calls)[0] != want {
		t.Fatalf("dir chown calls = %v, want [%v]", *calls, want)
	}
	if want := (chownCall{"", uid, gid}); len(fileCalls) != 1 || fileCalls[0] != want {
		t.Fatalf("file chown calls = %v, want [%v]", fileCalls, want)
	}
}
