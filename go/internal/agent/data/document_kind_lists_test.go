package data

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/datadoc"
)

func TestKindListsComeFromTheRegistry(t *testing.T) {
	if got := strings.Join(DocumentKinds(), ","); got != strings.Join(documentKinds.Kinds(), ",") {
		t.Fatalf("DocumentKinds() = %s", got)
	}
	stored := StoredKinds()
	if slices.Contains(stored, datadoc.DefaultKind) || len(stored) != len(DocumentKinds())-1 {
		t.Fatalf("StoredKinds() = %v; want every kind but %s", stored, datadoc.DefaultKind)
	}
	if got := strings.Join(OwnableKinds(), ","); got != strings.Join(documentKinds.OwnableKinds(), ",") {
		t.Fatalf("OwnableKinds() = %s", got)
	}
}

func TestEveryStoredKindIsAcceptedByTheStore(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range StoredKinds() {
		if _, err := m.Documents(kind); err != nil {
			t.Fatalf("Documents(%q) = %v", kind, err)
		}
	}
	if _, err := m.Documents(datadoc.DefaultKind); !errors.Is(err, ErrNotStoredKind) {
		t.Fatalf("Documents(collection) = %v", err)
	}
}
