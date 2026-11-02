package netwire

import (
	"bytes"
	"fmt"
	"math"
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

func TestExtensibleArtifactValidEncodingBoundaries(t *testing.T) {
	metadata := make(map[string]any, 64)
	for i := range 64 {
		metadata[fmt.Sprintf("field%02d", i)] = uint64(i)
	}
	var nested any = true
	for range 15 { // The enclosing artifact map is the sixteenth container.
		nested = []any{nested}
	}
	values := map[string]any{
		"integerWidths": []any{uint64(23), uint64(24), uint64(255), uint64(256),
			uint64(65535), uint64(65536), uint64(math.MaxUint32), uint64(math.MaxUint32) + 1,
			uint64(math.MaxUint64), int64(math.MinInt64)},
		"mapPairs":      metadata,
		"arrayElements": make([]uint64, 1024),
		"nesting":       nested,
		"textWidths":    []string{string(bytes.Repeat([]byte("a"), 24)), string(bytes.Repeat([]byte("b"), 256)), "未来"},
		"byteWidths":    [][]byte{bytes.Repeat([]byte{0xff}, 24), bytes.Repeat([]byte{0}, 256)},
	}
	for name, value := range values {
		t.Run(name, func(t *testing.T) {
			data, err := Marshal(map[string]any{"n": "known", "future": value})
			if err != nil {
				t.Fatal(err)
			}
			var view struct {
				Name string `cbor:"n"`
			}
			if err := UnmarshalExtensibleArtifact(data, &view, len(data)); err != nil {
				t.Fatal(err)
			}
			if view.Name != "known" {
				t.Fatalf("known field changed: %+v", view)
			}
			var raw RawMessage
			if err := UnmarshalExtensibleArtifact(data, &raw, len(data)); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, data) {
				t.Fatal("raw artifact changed")
			}
		})
	}
}
