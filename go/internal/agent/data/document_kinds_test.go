package data

import (
	"strings"
	"testing"
	"time"
)

const testInferenceDocument = `kind: inference
version: 1
name: front-door
sources:
  - camera: v4l2:/dev/video0
model: ustc-community/dfine-nano-coco
revision: 066438d3d8f0da137a37b38fdf3368fd4afceced
labels: [person]
threshold: 0.5
rate: 2
clear_after: 5s
cooldown: 30s
`

const testWatchDocument = `kind: watch
version: 1
name: front-door
lease: 60s
on:
  inference: front-door
notify:
  - cloud
  - webhook: https://example.com/hooks/front-door
`

func TestInferenceDocumentParses(t *testing.T) {
	doc, err := ParseDocument([]byte(testInferenceDocument))
	if err != nil {
		t.Fatal(err)
	}
	inference, ok := doc.Body.(InferenceDocument)
	if !ok {
		t.Fatalf("body is %T", doc.Body)
	}
	if inference.Event != "front-door" {
		t.Fatalf("event = %q, want the document name", inference.Event)
	}
	if inference.Model != "ustc-community/dfine-nano-coco" || inference.Sources[0].Camera != "v4l2:/dev/video0" {
		t.Fatalf("inference = %+v", inference)
	}
	if doc.Revision == "" {
		t.Fatal("no revision")
	}
	campaign := inference.Campaign(doc.Revision)
	if campaign.Name != "front-door" || campaign.State != "armed" || campaign.Revision != doc.Revision || !campaign.Inference.IsEnabled() || campaign.Inference.Event != "front-door" {
		t.Fatalf("job campaign = %+v", campaign)
	}
}

func TestInferenceDocumentRevisionFollowsTheAuthor(t *testing.T) {
	first, err := ParseDocument([]byte(testInferenceDocument))
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseDocument([]byte("# a comment changes nothing\n" + testInferenceDocument))
	if err != nil {
		t.Fatal(err)
	}
	changed, err := ParseDocument([]byte(strings.Replace(testInferenceDocument, "threshold: 0.5", "threshold: 0.6", 1)))
	if err != nil {
		t.Fatal(err)
	}
	owned, err := ParseDocument([]byte(testInferenceDocument + "owner: watch/front-door\n"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != again.Revision {
		t.Fatal("a comment changed the revision")
	}
	if first.Revision == changed.Revision || first.Revision == owned.Revision {
		t.Fatal("an author change kept the revision")
	}
}

func TestInferenceDocumentErrors(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"no sources":        {"sources:\n  - camera: v4l2:/dev/video0\n", "sources: []\n", "at least one camera"},
		"audio source":      {"  - camera: v4l2:/dev/video0\n", "  - audio: cabin-mic\n", "cameras only"},
		"capture on source": {"  - camera: v4l2:/dev/video0\n", "  - camera: v4l2:/dev/video0\n    capture: {mode: snapshot, interval: 2s}\n", "cameras only"},
		"unpinned model":    {"revision: 066438d3d8f0da137a37b38fdf3368fd4afceced", "revision: main", "40-character"},
		"unknown field":     {"rate: 2\n", "rate: 2\nupload: {when: wifi}\n", "field upload not found"},
		"bad event":         {"rate: 2\n", "rate: 2\nevent: front door\n", "inference.event"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(testInferenceDocument, tc.from) {
				t.Fatalf("fixture lacks %q", tc.from)
			}
			_, err := ParseDocument([]byte(strings.Replace(testInferenceDocument, tc.from, tc.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestWatchParses(t *testing.T) {
	doc, err := ParseDocument([]byte(testWatchDocument))
	if err != nil {
		t.Fatal(err)
	}
	watch, ok := doc.Body.(Watch)
	if !ok {
		t.Fatalf("body is %T", doc.Body)
	}
	if !watch.Leased() || watch.LeaseDuration() != time.Minute || watch.On.Inference != "front-door" {
		t.Fatalf("watch = %+v", watch)
	}
	if !watch.NotifiesCloud() || watch.WebhookURL() != "https://example.com/hooks/front-door" {
		t.Fatalf("notify = %+v", watch.Notify)
	}
	plain, err := ParseDocument([]byte("kind: watch\nversion: 1\nname: porch\non: {inference: porch}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if w := plain.Body.(Watch); w.Leased() || w.NotifiesCloud() || w.WebhookURL() != "" {
		t.Fatalf("plain watch = %+v", w)
	}
}

func TestWatchErrors(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"lease too short":  {"lease: 60s", "lease: 5s", "lease must be a duration from 15s to 10m"},
		"lease too long":   {"lease: 60s", "lease: 11m", "lease must be a duration from 15s to 10m"},
		"lease not a time": {"lease: 60s", "lease: soon", "lease must be a duration from 15s to 10m"},
		"empty lease":      {"lease: 60s", "lease: \"\"", "lease must be a duration from 15s to 10m"},
		"bare lease":       {"lease: 60s", "lease:", "lease must be a duration from 15s to 10m"},
		"empty webhook":    {"https://example.com/hooks/front-door", "", "a notify entry is `cloud` or `webhook: <url>`"},
		"no inference":     {"  inference: front-door\n", "  inference: \"\"\n", "on.inference must name an inference document"},
		"bad inference":    {"  inference: front-door\n", "  inference: front door\n", "on.inference must name an inference document"},
		"filter on watch":  {"  inference: front-door\n", "  inference: front-door\n  labels: [person]\n", "field labels not found"},
		"unknown notify":   {"  - cloud\n", "  - email\n", "a notify entry is `cloud` or `webhook: <url>`"},
		"two clouds":       {"  - cloud\n", "  - cloud\n  - cloud\n", "at most once"},
		"bad webhook":      {"https://example.com/hooks/front-door", "ftp://example.com/x", "notify webhook must be an absolute HTTP(S) URL"},
		"webhook userinfo": {"https://example.com/hooks/front-door", "https://user:pw@example.com/x", "notify webhook must be an absolute HTTP(S) URL"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(testWatchDocument, tc.from) {
				t.Fatalf("fixture lacks %q", tc.from)
			}
			_, err := ParseDocument([]byte(strings.Replace(testWatchDocument, tc.from, tc.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCampaignWebhookRuleIsUnchanged(t *testing.T) {
	raw, err := ParseCampaign(inferenceCampaignWithWebhook("ftp://example.com/x"))
	if err == nil || !strings.Contains(err.Error(), "notify.webhook must be an absolute HTTP(S) URL without userinfo or fragment, at most 2048 bytes") {
		t.Fatalf("campaign webhook error = %v (%+v)", err, raw)
	}
}

func inferenceCampaignWithWebhook(webhook string) []byte {
	return []byte(`version: 1
name: people
sources: [{camera: "*"}]
inference:
  model: facebook/detr-resnet-50
  revision: 1d5f47bd3bdd2c4bbfa585418ffe6da5028b4c0b
  labels: [person]
  threshold: 0.9
  rate: 1
  event: person_detected
  clear_after: 5s
  cooldown: 30s
capture: {buffer: 0s, after_trigger: 1s, triggers: [{event: person_detected}]}
upload: {when: manual}
export: {annotation: cvat}
notify: {on: detection, webhook: "` + webhook + `"}
`)
}
