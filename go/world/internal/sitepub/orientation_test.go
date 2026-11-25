package sitepub

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestLegacyGeoreferenceContentIdentityUnchanged(t *testing.T) {
	raw, cfg := fixture(t)
	c := mustBuild(t, raw, cfg)
	// Captured from the v1 implementation at dd31c599 before adding v2 fields.
	const want = "site-georeference/2bd9fe5c28112f637686eab170173bd86242574f68ab55759a72ef26da8644e2"
	if c.Key.Entity != want {
		t.Fatalf("legacy canonical document changed: %s", c.Key.Entity)
	}
	if bytes.Contains(c.Payload, []byte("rotation_error")) || bytes.Contains(c.Payload, []byte("root_basis_error")) {
		t.Fatal("legacy document gained orientation assertions")
	}
	if _, err := Validate(clone(t, c)); err != nil {
		t.Fatal(err)
	}
}

func TestOrientationPublicationPreservesTaggedBounds(t *testing.T) {
	_, cfg := fixture(t)
	raw, err := os.ReadFile("../../examples/geography/office-orientation-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, unknown := range []bool{false, true} {
		var document map[string]any
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatal(err)
		}
		if unknown {
			document["root_basis_error"] = map[string]any{"known": false}
			document["frames"].([]any)[1].(map[string]any)["rotation_error"] = map[string]any{"known": false}
		}
		input, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		c := mustBuild(t, input, cfg)
		p, err := Validate(clone(t, c))
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(p.Document, &got); err != nil {
			t.Fatal(err)
		}
		wantBytes, _ := json.Marshal(document)
		gotBytes, _ := json.Marshal(got)
		if !bytes.Equal(wantBytes, gotBytes) {
			t.Fatal("transport changed a geographic assertion")
		}
		if c.Key.Entity == "site-georeference/2bd9fe5c28112f637686eab170173bd86242574f68ab55759a72ef26da8644e2" {
			t.Fatal("new document reused the legacy identity")
		}
	}
}
