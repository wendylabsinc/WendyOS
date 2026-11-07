package data

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func saveTestDocument(t *testing.T, m *Manager, contents string) StoredDocument {
	t.Helper()
	doc, err := ParseDocument([]byte(contents))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := m.SaveDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestDocumentStoreRoundTrip(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stored := saveTestDocument(t, m, testWatchDocument)
	if stored.Ref() != "watch/front-door" || stored.Revision == "" || stored.DeployedUnixNanos == 0 {
		t.Fatalf("stored = %+v", stored)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(m.root), "documents", "watch", "front-door.json")); err != nil {
		t.Fatalf("document file: %v", err)
	}
	loaded, err := m.Document(KindWatch, "front-door")
	if err != nil {
		t.Fatal(err)
	}
	watch, err := loaded.Watch()
	if err != nil || !watch.Leased() || watch.On.Inference != "front-door" || !watch.NotifiesCloud() {
		t.Fatalf("loaded watch = %+v, %v", watch, err)
	}
	if loaded.Revision != stored.Revision {
		t.Fatal("revision changed in storage")
	}
	inference := saveTestDocument(t, m, testInferenceDocument)
	document, err := inference.Inference()
	if err != nil || document.Event != "front-door" || document.Model != "ustc-community/dfine-nano-coco" {
		t.Fatalf("loaded inference = %+v, %v", document, err)
	}
	if _, err := inference.Watch(); err == nil {
		t.Fatal("an inference document decoded as a watch")
	}
}

func TestDocumentsListsByKindSkippingUnreadable(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var warnings []string
	m.SetWarnLogger(func(msg string) { warnings = append(warnings, msg) })
	saveTestDocument(t, m, strings.Replace(testWatchDocument, "name: front-door", "name: zz", 1))
	saveTestDocument(t, m, testWatchDocument)
	saveTestDocument(t, m, testInferenceDocument)
	if err := os.WriteFile(filepath.Join(filepath.Dir(m.root), "documents", "watch", "broken.json"), []byte("{"), 0o640); err != nil {
		t.Fatal(err)
	}
	watches, err := m.Documents(KindWatch)
	if err != nil {
		t.Fatal(err)
	}
	if len(watches) != 2 || watches[0].Name != "front-door" || watches[1].Name != "zz" {
		t.Fatalf("watches = %+v", watches)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "broken") {
		t.Fatalf("warnings = %v", warnings)
	}
	// The data directory is the root's parent, so the empty store gets its own
	// parent: a sibling TempDir would share m's documents directory.
	none, err := NewManager(filepath.Join(t.TempDir(), "episodes"))
	if err != nil {
		t.Fatal(err)
	}
	if empty, err := none.Documents(KindInference); err != nil || len(empty) != 0 {
		t.Fatalf("empty store = %v, %v", empty, err)
	}
}

func TestDocumentStoreErrors(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Document(KindWatch, "missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing document error = %v", err)
	}
	if err := m.RemoveDocument(KindWatch, "missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing removal error = %v", err)
	}
	if _, err := m.Document(KindWatch, "../escape"); !errors.Is(err, ErrInvalidDocumentName) {
		t.Fatalf("escaping name error = %v", err)
	}
	if _, err := m.Documents("collection"); !errors.Is(err, ErrNotStoredKind) {
		t.Fatalf("collection listing error = %v", err)
	}
	collection, err := ParseDocument(minimalCampaign(1, "telemetry: true", "wifi"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.SaveDocument(collection); !errors.Is(err, ErrNotStoredKind) {
		t.Fatalf("saving a collection error = %v", err)
	}
	saveTestDocument(t, m, testWatchDocument)
	if err := m.RemoveDocument(KindWatch, "front-door"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Document(KindWatch, "front-door"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed document error = %v", err)
	}
}
