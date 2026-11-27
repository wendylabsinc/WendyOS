package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReadTokenRequiresBoundedPrivateRegularFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	if e := os.WriteFile(p, []byte(strings.Repeat("a", 32)), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := readToken(p); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(p, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := readToken(p); e == nil {
		t.Fatal("read public token")
	}
	if e := os.Chmod(p, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(strings.Repeat("a", 257)), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := readToken(p); e == nil {
		t.Fatal("read oversized token")
	}
	pipe := filepath.Join(dir, "pipe")
	if e := syscall.Mkfifo(pipe, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := readToken(pipe); e == nil {
		t.Fatal("read FIFO")
	}
}
