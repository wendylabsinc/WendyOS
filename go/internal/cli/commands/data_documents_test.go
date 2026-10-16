package commands

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDocumentFilePickerOffersEveryKind(t *testing.T) {
	t.Chdir(t.TempDir())
	writePickerFiles(t, map[string]string{
		"clips.collection.yaml": "version: 1\nname: clips\n",
		"door.inference.yaml":   "kind: inference\nversion: 1\nname: door\n",
		"door.watch.yaml":       "kind: watch\nversion: 1\nname: door\n",
		"compose.yml":           "services: {}\n",
	})
	path, err := documentFilePicker.resolve(nil, true, func(_ string, items []tui.PickerItem) (string, error) {
		var labels []string
		for _, item := range items {
			labels = append(labels, item.Name)
		}
		want := []string{"clips.collection.yaml (collection clips)", "door.inference.yaml (inference door)", "door.watch.yaml (watch door)"}
		if !reflect.DeepEqual(labels, want) {
			t.Fatalf("labels = %v", labels)
		}
		return items[2].Value.(string), nil
	})
	if err != nil || path != "door.watch.yaml" {
		t.Fatalf("picked %q: %v", path, err)
	}
	_, err = documentFilePicker.resolve(nil, false, nil)
	if err == nil || !strings.Contains(err.Error(), "wendy data apply") || !strings.Contains(err.Error(), "door.watch.yaml") || strings.Contains(err.Error(), "compose.yml") {
		t.Fatalf("non-interactive guidance = %v", err)
	}
}

func TestParseDocumentFilterAndRef(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		kind, name string
		ok         bool
	}{
		{nil, "", "", true},
		{[]string{"watch"}, "watch", "", true},
		{[]string{"watch/front-door"}, "watch", "front-door", true},
		{[]string{"collection/clips"}, "collection", "clips", true},
		{[]string{"robot"}, "", "", false},
		{[]string{"watch/front door"}, "", "", false},
		{[]string{"watch/"}, "", "", false},
	} {
		kind, name, err := parseDocumentFilter(tc.args)
		if (err == nil) != tc.ok || kind != tc.kind || name != tc.name {
			t.Errorf("parseDocumentFilter(%v) = %q, %q, %v", tc.args, kind, name, err)
		}
	}
	if _, _, err := parseDocumentRef("watch"); err == nil {
		t.Error("a bare kind was accepted as a reference")
	}
	if kind, name, err := parseDocumentRef("inference/door"); err != nil || kind != "inference" || name != "door" {
		t.Errorf("parseDocumentRef = %q, %q, %v", kind, name, err)
	}
}

func TestDocumentCloudApps(t *testing.T) {
	cases := map[string][]string{
		"version: 1\nname: clips\nsources: [{telemetry: true}]\ncapture: {buffer: 0s, after_trigger: 1s, triggers: [{event: x}]}\nupload: {when: manual}\nexport: {annotation: cvat}\n": {"campaign:clips"},
		"kind: watch\nversion: 1\nname: porch\non: {inference: door}\nnotify: [cloud]\n":                                                                                                {"watch:porch"},
		"kind: watch\nversion: 1\nname: chat-1\nlease: 60s\non: {inference: chat-1}\n":                                                                                                  nil,
	}
	for contents, want := range cases {
		document, err := data.ParseDocument([]byte(contents))
		if err != nil {
			t.Fatal(err)
		}
		if got := documentCloudApps(document); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: apps = %v, want %v", document.Header.Name, got, want)
		}
	}
}

func TestDocumentCallErrorOnOldAgents(t *testing.T) {
	err := documentCallError(status.Error(codes.Unimplemented, "unknown method DocumentApply"))
	if err == nil || !strings.Contains(err.Error(), "wendy device update") {
		t.Fatalf("old agent error = %v", err)
	}
	other := status.Error(codes.NotFound, "no document named \"x\"")
	if documentCallError(other) != other {
		t.Fatal("an unrelated error was rewritten")
	}
}

func TestCampaignDeployPointsOtherKindsAtApply(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("door.watch.yaml", []byte("kind: watch\nversion: 1\nname: door\non: {inference: door}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newDataCampaignDeployCmd()
	cmd.SetArgs([]string{"door.watch.yaml"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "wendy data apply door.watch.yaml") {
		t.Fatalf("campaign deploy of a watch = %v", err)
	}
}
