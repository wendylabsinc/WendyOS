package twin

import (
	"crypto/sha256"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	"testing"
	"time"
)

func TestLegacyReferencesMigrateOnceAndSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewStore(dir)
	r := room()
	r.Model = []byte("PK\x03\x04original-model")
	r, err := store.Put(r)
	if err != nil {
		t.Fatal(err)
	}
	input := ReferenceImport{ModelRevision: r.Site.ModelRevision, ModelDigest: fmt.Sprintf("%x", sha256.Sum256(r.Model)), Robot: "go2", SavedAt: time.Now().Add(-time.Hour), Surfaces: []sitenav.Surface{{Position: sitenav.Vertex{X: 1}, Direction: sitenav.Point{X: 1}, Stable: true}}}
	for _, kind := range []string{"digest", "frame", "expired", "direction"} {
		wrong := input
		wrong.Surfaces = append([]sitenav.Surface(nil), input.Surfaces...)
		switch kind {
		case "digest":
			wrong.ModelDigest = "wrong"
		case "frame":
			wrong.ModelRevision = "wrong"
		case "expired":
			wrong.SavedAt = time.Now().Add(-8 * 24 * time.Hour)
		case "direction":
			wrong.Surfaces[0].Direction.X = 10
		}
		if err := store.ImportReferences(r.Site.ID, wrong); err == nil {
			t.Fatal("accepted", kind)
		}
	}
	if err := store.ImportReferences(r.Site.ID, input); err != nil {
		t.Fatal(err)
	}
	imported, _ := store.Get(r.Site.ID)
	input.Surfaces[0].Position.X = 99
	if err := store.ImportReferences(r.Site.ID, input); err != nil {
		t.Fatal(err)
	}
	restarted, _ := NewStore(dir)
	saved, _ := restarted.Get(r.Site.ID)
	if len(saved.References["go2"]) != 1 || saved.References["go2"][0].Position.X != 1 || saved.Revision != imported.Revision {
		t.Fatal("migration replaced references or failed persistence")
	}
}
