package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemoryLookupWorksBeyondSearchLimitAndAcrossContexts(t *testing.T) {
	store := newTestMemoryStore(t)
	want := saveTestMemory(t, store, testMemoryInput("Old workspace note"))
	for i := range 50 {
		saveTestMemory(t, store, testMemoryInput(fmt.Sprintf("Newer note %d", i)))
	}
	entries, err := store.Search(context.Background(), "", "", 50)
	if err != nil || len(entries) != 50 {
		t.Fatalf("search: %d entries, %v", len(entries), err)
	}
	for _, entry := range entries {
		if entry.ID == want.ID {
			t.Fatal("fixture's old note is still within the search limit")
		}
	}
	other, err := NewMemoryStore(store.Directory(), t.TempDir(), "Rover")
	if err != nil {
		t.Fatal(err)
	}
	device := saveTestMemory(t, store, MemoryInput{
		Scope: "device", Device: "Woof", Kind: "fact", Title: "Woof camera", Content: "Use the front camera.", Evidence: "The user supplied this preference.",
	})
	for _, source := range []*MemoryStore{store, other} {
		for _, want := range []MemoryEntry{want, device} {
			for _, length := range []int{8, 12, 63, 64} {
				got, err := source.Lookup(context.Background(), want.ID[:length])
				if err != nil || got != want {
					t.Fatalf("lookup %s: %+v, %v", want.ID[:length], got, err)
				}
			}
		}
	}
}

func TestMemoryLookupRejectsAmbiguousPrefixes(t *testing.T) {
	store := newTestMemoryStore(t)
	// These global titles have canonical SHA-256 IDs sharing the prefix 194b08a2.
	var entries []MemoryEntry
	for _, title := range []string{"Lookup collision 15669", "Lookup collision 135542"} {
		input := testMemoryInput(title)
		input.Scope = "global"
		entries = append(entries, saveTestMemory(t, store, input))
	}
	if entries[0].ID[:8] != entries[1].ID[:8] || entries[0].ID[:9] == entries[1].ID[:9] {
		t.Fatal("collision fixture is invalid")
	}
	_, err := store.Lookup(context.Background(), entries[0].ID[:8])
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous prefix: %v", err)
	}
	for _, entry := range entries {
		if !strings.Contains(err.Error(), entry.ID) || !strings.Contains(err.Error(), entry.Title) {
			t.Fatalf("ambiguous prefix does not identify each choice: %v", err)
		}
	}
	for _, want := range entries {
		got, err := store.Lookup(context.Background(), want.ID[:9])
		if err != nil || got.ID != want.ID {
			t.Fatalf("longer prefix: %+v, %v", got, err)
		}
	}
	// An invalid document sharing a prefix must not make a valid note ambiguous.
	if err := os.WriteFile(filepath.Join(store.Directory(), entries[1].ID+".json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := store.Lookup(context.Background(), entries[0].ID[:8])
	if err != nil || got.ID != entries[0].ID {
		t.Fatalf("invalid collision contaminated lookup: %+v, %v", got, err)
	}
	if _, err := store.Lookup(context.Background(), entries[1].ID); err == nil {
		t.Fatal("exact lookup accepted an invalid document")
	}
}

func TestMemoryLookupInvalidMissingAndCanceled(t *testing.T) {
	store := newTestMemoryStore(t)
	for _, ref := range []string{"", "abcdef0", "ABCDEFGH", "abcdefgz", "../outside", strings.Repeat("a", 65)} {
		if _, err := store.Lookup(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "lowercase hexadecimal") {
			t.Errorf("invalid reference %q: %v", ref, err)
		}
	}
	for _, ref := range []string{"abcdef01", strings.Repeat("0", 64)} {
		if _, err := store.Lookup(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "no remembered note matches") {
			t.Errorf("missing directory, ref %q: %v", ref, err)
		}
	}
	if _, err := os.Stat(store.Directory()); !os.IsNotExist(err) {
		t.Fatalf("lookup created storage: %v", err)
	}
	entry := saveTestMemory(t, store, testMemoryInput("Existing note"))
	for _, ref := range []string{"abcdef01", strings.Repeat("0", 64)} {
		if _, err := store.Lookup(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "no remembered note matches") {
			t.Errorf("missing note, ref %q: %v", ref, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ref := range []string{entry.ID, entry.ID[:8]} {
		if _, err := store.Lookup(ctx, ref); !errors.Is(err, context.Canceled) {
			t.Errorf("canceled lookup: %v", err)
		}
	}
}

func TestMemoryLookupRejectsSymlinks(t *testing.T) {
	store := newTestMemoryStore(t)
	entry := saveTestMemory(t, store, testMemoryInput("Linked note"))
	path := filepath.Join(store.Directory(), entry.ID+".json")
	target := filepath.Join(t.TempDir(), entry.ID+".json")
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, ref := range []string{entry.ID, entry.ID[:8]} {
		if _, err := store.Lookup(context.Background(), ref); err == nil {
			t.Errorf("followed symlink note for %q", ref)
		}
	}
	alias := filepath.Join(t.TempDir(), "memory-link")
	if err := os.Symlink(store.Directory(), alias); err != nil {
		t.Fatal(err)
	}
	linked, err := NewMemoryStore(alias, store.workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{entry.ID, entry.ID[:8]} {
		if _, err := linked.Lookup(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("followed symlink directory for %q: %v", ref, err)
		}
	}
}

func TestMemoryLookupBoundsPrefixScansButReadsExactIDsDirectly(t *testing.T) {
	store := newTestMemoryStore(t)
	want := saveTestMemory(t, store, testMemoryInput("Known note"))
	for i := range memoryMaxFiles {
		if err := os.WriteFile(filepath.Join(store.Directory(), fmt.Sprintf("unrelated-%d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Lookup(context.Background(), want.ID[:8]); err == nil || !strings.Contains(err.Error(), "scan limit") {
		t.Fatalf("unbounded prefix scan: %v", err)
	}
	got, err := store.Lookup(context.Background(), want.ID)
	if err != nil || got.ID != want.ID {
		t.Fatalf("exact lookup unnecessarily scanned the directory: %+v, %v", got, err)
	}
}
