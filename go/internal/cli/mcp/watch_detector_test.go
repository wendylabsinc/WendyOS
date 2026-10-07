package mcp

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The embedded labels must be the checkpoint's own, in class order: watch_start
// accepts exactly these names, and the agent rejects labels the model lacks.
func TestDefaultWatchDetectorLabelsMatchCheckpoint(t *testing.T) {
	raw, err := os.ReadFile("testdata/dfine-nano-coco-id2label.json")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		ID2Label map[string]string `json:"id2label"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.ID2Label) != len(defaultWatchDetector.Labels) {
		t.Fatalf("detector has %d labels, checkpoint has %d", len(defaultWatchDetector.Labels), len(config.ID2Label))
	}
	for i, label := range defaultWatchDetector.Labels {
		if want := config.ID2Label[strconv.Itoa(i)]; label != want {
			t.Fatalf("label %d = %q, checkpoint says %q", i, label, want)
		}
	}
	if !defaultWatchDetector.hasLabel("motorbike") || defaultWatchDetector.hasLabel("motorcycle") {
		t.Fatal("labels must be the checkpoint's spelling, not COCO's")
	}
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(defaultWatchDetector.Revision) || defaultWatchDetector.Model != "ustc-community/dfine-nano-coco" {
		t.Fatalf("detector is not pinned: %+v", defaultWatchDetector)
	}
}
