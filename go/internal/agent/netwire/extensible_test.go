package netwire

import (
	"bytes"
	"testing"
)

func TestExtensibleArtifactOptionalMetadata(t *testing.T) {
	data, err := Marshal(map[string]any{
		"version": uint64(4), "route": map[string]any{"name": "known", "future": map[string]any{
			"ready": true, "data": []byte{1, 2}, "delta": int64(-3), "optional": nil, "items": []any{"hello", uint64(42)},
		}}, "futureTop": "description",
	})
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Version uint64 `json:"version"`
		Route   struct {
			Name string `json:"name"`
		} `json:"route"`
	}
	if err := UnmarshalExtensibleArtifact(data, &view, 4096); err != nil {
		t.Fatal(err)
	}
	if view.Version != 4 || view.Route.Name != "known" {
		t.Fatalf("local view: %+v", view)
	}
	var raw map[string]RawMessage
	if err := UnmarshalExtensibleArtifact(data, &raw, 4096); err != nil {
		t.Fatal(err)
	}
	reencoded, err := Marshal(raw)
	if err != nil || !bytes.Equal(data, reencoded) {
		t.Fatalf("unknown metadata raw values changed: %x %v", reencoded, err)
	}
}
