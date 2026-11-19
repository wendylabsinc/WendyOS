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
	time.Sleep(100 * time.Millisecond) // let the watch be established

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
