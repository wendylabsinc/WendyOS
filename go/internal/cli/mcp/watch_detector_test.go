package mcp

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The embedded labels must be the checkpoint's own, in class order: watch_start
// accepts exactly these names, and the agent rejects labels the model lacks.
func TestFirstWatchDetectorLabelsMatchCheckpoint(t *testing.T) {
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
	if len(config.ID2Label) != len(watchDetectors[0].Labels) {
		t.Fatalf("detector has %d labels, checkpoint has %d", len(watchDetectors[0].Labels), len(config.ID2Label))
	}
	for i, label := range watchDetectors[0].Labels {
		if want := config.ID2Label[strconv.Itoa(i)]; label != want {
			t.Fatalf("label %d = %q, checkpoint says %q", i, label, want)
		}
	}
	if !watchDetectors[0].hasLabel("motorbike") || watchDetectors[0].hasLabel("motorcycle") {
		t.Fatal("labels must be the checkpoint's spelling, not COCO's")
	}
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(watchDetectors[0].Revision) || watchDetectors[0].Model != "ustc-community/dfine-nano-coco" {
		t.Fatalf("detector is not pinned: %+v", watchDetectors[0])
	}
}

func TestEveryDetectorMeetsTheEntryRules(t *testing.T) {
	revision := regexp.MustCompile(`^[0-9a-f]{40}$`)
	for _, d := range watchDetectors {
		if d.ID == "" || !strings.Contains(d.Model, "/") || !revision.MatchString(d.Revision) || len(d.Labels) == 0 || d.License == "" || d.Weights == "" || d.Benchmark == "" {
			t.Errorf("detector %+v lacks an entry rule (spec §11.2)", d)
		}
	}
}

func TestDetectorForPicksTheFirstThatCoversTheClasses(t *testing.T) {
	d, ok := detectorFor([]string{"person", "dog"})
	if !ok || d.ID != "dfine-nano-coco" {
		t.Fatalf("detectorFor = %+v, %v", d, ok)
	}
	if _, ok := detectorFor([]string{"person", "forklift"}); ok {
		t.Fatal("a class no detector has was accepted")
	}
}
