//go:build linux

package localmesh

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func resolverFixture(t *testing.T) (*HostPolicy, resolverFiles, string) {
	t.Helper()
	dir := t.TempDir()
	f := resolverFiles{filepath.Join(dir, "resolv.conf"), filepath.Join(dir, "stub"), filepath.Join(dir, "journal")}
	previous := "original-resolv.conf"
	if err := os.WriteFile(f.stub, []byte("nameserver 127.0.0.53\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(previous, f.link); err != nil {
		t.Fatal(err)
	}
	return &HostPolicy{resolverFiles: &f}, f, previous
}

func assertResolverLink(t *testing.T, f resolverFiles, want string) {
	t.Helper()
	got, err := os.Readlink(f.link)
	if err != nil || got != want {
		t.Fatalf("resolver link = %q, %v; want %q", got, err, want)
	}
}

func TestResolverJournalRecoversAfterProcessExit(t *testing.T) {
	p, f, previous := resolverFixture(t)
	if err := p.useResolverStub(); err != nil {
		t.Fatal(err)
	}
	assertResolverLink(t, f, f.stub)
	info, err := os.Stat(f.journal)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private journal missing: %v", err)
	}
	// A fresh policy has no in-memory resolver owner. Close must still recover it.
	fresh := &HostPolicy{resolverFiles: &f}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	assertResolverLink(t, f, previous)
	if _, err := os.Stat(f.journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal retained: %v", err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResolverJournalRecoversCrashBeforeLinkReplacement(t *testing.T) {
	p, f, previous := resolverFixture(t)
	data, _ := json.Marshal(resolverJournal{Version: 1, Previous: previous})
	if err := os.WriteFile(f.journal, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	assertResolverLink(t, f, previous)
}

func TestResolverJournalPreservesForeignChangeAndRetries(t *testing.T) {
	p, f, previous := resolverFixture(t)
	if err := p.useResolverStub(); err != nil {
		t.Fatal(err)
	}
	if err := replaceResolverLink(f.link, "foreign-resolv.conf"); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err == nil {
		t.Fatal("foreign resolver was overwritten")
	}
	assertResolverLink(t, f, "foreign-resolv.conf")
	if _, err := os.Stat(f.journal); err != nil {
		t.Fatal("failed cleanup lost its recovery journal")
	}
	if err := replaceResolverLink(f.link, f.stub); err != nil {
		t.Fatal(err)
	}
	if err := p.SetDNS(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
	assertResolverLink(t, f, previous)
}

func TestResolverJournalFailurePreventsMutation(t *testing.T) {
	p, f, previous := resolverFixture(t)
	p.resolverFiles.journal = filepath.Join(f.journal, "missing", "journal")
	if err := p.useResolverStub(); err == nil {
		t.Fatal("changed resolver without durable ownership")
	}
	assertResolverLink(t, f, previous)
}

func TestResolverJournalRejectsCorruption(t *testing.T) {
	for _, data := range []string{"{", `{"version":2,"previous":"old"}`, `{"version":1,"previous":""}`} {
		p, f, previous := resolverFixture(t)
		if err := os.WriteFile(f.journal, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := p.Close(); err == nil {
			t.Fatal("corrupt ownership was accepted")
		}
		assertResolverLink(t, f, previous)
	}
}

func TestResolverAlreadyOnStubIsNotOwned(t *testing.T) {
	p, f, _ := resolverFixture(t)
	if err := replaceResolverLink(f.link, f.stub); err != nil {
		t.Fatal(err)
	}
	if err := p.useResolverStub(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	assertResolverLink(t, f, f.stub)
	if _, err := os.Stat(f.journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("claimed a pre-existing stub: %v", err)
	}
}
