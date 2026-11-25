//go:build unix

package commands

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRenderAndWriteTemplate_ForcePreservesExistingPermissions(t *testing.T) {
	dir := t.TempDir()
	for name, mode := range map[string]os.FileMode{"report.sh": 0o755, "wendy.json": 0o600} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("old content\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]byte{
		"report.sh":  []byte("#!/bin/sh\nprintf 'template report\\n'\n"),
		"wendy.json": []byte(`{"appId":"{{.APP_ID}}"}`),
	}
	if err := renderAndWriteTemplate(files, dir, "demo", "fixture", map[string]interface{}{"APP_ID": "demo"}, true); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"report.sh": 0o755, "wendy.json": 0o600} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s: mode %04o, want %04o", name, info.Mode().Perm(), want)
		}
	}
	out, err := exec.Command(filepath.Join(dir, "report.sh")).CombinedOutput()
	if err != nil {
		t.Errorf("rendered script cannot execute: %v; output %q", err, out)
	}
	if string(out) != "template report\n" {
		t.Errorf("script did not execute template content: %q", out)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "wendy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(manifest) != `{"appId":"demo"}` {
		t.Fatalf("restricted manifest was not rendered: %q", manifest)
	}
}

func TestRenderAndWriteTemplate_NewFilesRespectUmask(t *testing.T) {
	previous := syscall.Umask(0o077)
	defer syscall.Umask(previous)
	for _, force := range []bool{false, true} {
		dir := t.TempDir()
		files := map[string][]byte{"README.md": []byte("private project notes\n")}
		if err := renderAndWriteTemplate(files, dir, "demo", "fixture", nil, force); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(dir, "README.md"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("force=%t: new README mode %04o bypasses umask 077", force, info.Mode().Perm())
		}
		got, err := os.ReadFile(filepath.Join(dir, "README.md"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "private project notes\n" {
			t.Fatalf("new file contents changed: %q", got)
		}
	}
}
