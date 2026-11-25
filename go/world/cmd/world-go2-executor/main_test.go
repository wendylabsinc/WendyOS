package main

import (
	"bytes"
	"github.com/wendylabsinc/wendy/go/world/internal/go2executor"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestConfigurationFileBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential")
	if err := os.WriteFile(path, []byte("private simulation fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(path, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(path, true); err == nil {
		t.Fatal("world-readable credential accepted")
	}
	if _, err := readFile(path, false); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(link, false); err == nil {
		t.Fatal("symbolic link accepted")
	}
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(fifo, true); err == nil {
		t.Fatal("named pipe accepted")
	}
	if _, err := readFile(dir, false); err == nil {
		t.Fatal("directory accepted")
	}
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, bytes.Repeat([]byte{'x'}, 65537), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(large, false); err == nil {
		t.Fatal("oversized configuration accepted")
	}
}

func TestFaultDiagnosticExclusiveAndBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fault.json")
	if err := writeFault(p, []byte(`{"simulation":true}`)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFault(p, []byte(`{"replacement":true}`)); err == nil {
		t.Fatal("replaced first fault")
	}
	after, _ := os.ReadFile(p)
	info, _ := os.Stat(p)
	if !bytes.Equal(before, after) || info.Mode().Perm() != 0600 {
		t.Fatal("fault file modified or not private")
	}
	if err := writeFault(p+"-large", make([]byte, go2executor.MaxFaultJSONBytes+1)); err == nil {
		t.Fatal("unbounded snapshot accepted")
	}
	if _, err := os.Stat(p + "-large"); !os.IsNotExist(err) {
		t.Fatal("oversized file created")
	}
}
