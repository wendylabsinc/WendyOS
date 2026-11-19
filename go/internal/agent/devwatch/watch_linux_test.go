//go:build linux

package devwatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatch_ReportsVideoNodesOnly(t *testing.T) {
	dir := t.TempDir()
	events := make(chan Event, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- Watch(ctx, dir, func(e Event) { events <- e }) }()

	// Wait for the watch to be live: poke a sentinel node until its create
	// event arrives, then remove it and drain its delete event.
	sentinel := filepath.Join(dir, "video999")
	for ready := false; !ready; {
		if err := os.WriteFile(sentinel, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		select {
		case e := <-events:
			ready = e == Event{Name: "video999", Created: true}
		case <-time.After(50 * time.Millisecond):
		}
		if err := os.Remove(sentinel); err != nil {
			t.Fatal(err)
		}
	}
	// Drain the sentinel's remaining events (stale creates and deletes).
	for quiet := false; !quiet; {
		select {
		case <-events:
		case <-time.After(200 * time.Millisecond):
			quiet = true
		}
	}

	expect := func(want Event) {
		t.Helper()
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("event = %+v, want %+v", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("no event, want %+v", want)
		}
	}
	expectNone := func(what string) {
		t.Helper()
		select {
		case got := <-events:
			t.Fatalf("%s produced event %+v", what, got)
		case <-time.After(200 * time.Millisecond):
		}
	}

	node := filepath.Join(dir, "video3")
	if err := os.WriteFile(node, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	expect(Event{Name: "video3", Created: true})

	if err := os.Chmod(node, 0o600); err != nil {
		t.Fatal(err)
	}
	expectNone("chmod video3")

	if err := os.WriteFile(filepath.Join(dir, "media0"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	expectNone("create media0")

	if err := os.Remove(node); err != nil {
		t.Fatal(err)
	}
	expect(Event{Name: "video3", Created: false})

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Watch returned %v after cancel, want nil", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Watch did not return within 200ms of cancel")
	}
}
