package data

import (
	"os"
	"path/filepath"
	"testing"
)

// The example documents are what docs point people at: each must parse, and
// the collection must trigger on the inference's event.
func TestWatchExamplesParse(t *testing.T) {
	files, err := filepath.Glob("../../../../Examples/WendyDataWatch/*.yaml")
	if err != nil || len(files) != 3 {
		t.Fatalf("example files = %v, %v", files, err)
	}
	kinds := map[string]any{}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := ParseDocument(raw)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		kinds[doc.Header.Kind] = doc.Body
	}
	inference := kinds[KindInference].(InferenceDocument)
	watch := kinds[KindWatch].(Watch)
	collection := kinds["collection"].(Campaign)
	if watch.On.Inference != inference.Name {
		t.Fatalf("the watch listens to %q, the inference is %q", watch.On.Inference, inference.Name)
	}
	triggered := false
	for _, trigger := range collection.Capture.Triggers {
		triggered = triggered || trigger.Event == inference.Event
	}
	if !triggered {
		t.Fatalf("the collection does not trigger on %q", inference.Event)
	}
}
