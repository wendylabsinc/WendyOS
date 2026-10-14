package services

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

const testInferenceDocumentYAML = `kind: inference
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

// newDocumentTestService is newInferenceTestService without the campaign: one
// healthy camera, a fake model runtime and a recording sender. The caller
// starts it, so a test can arrange the store first.
func newDocumentTestService(t *testing.T) (*DataService, *inferenceTestFactory, *inferenceTestSender) {
	t.Helper()
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	service.addAdapter(&inferenceTestAdapter{sources: []data.Source{{ID: "v4l2:/dev/video0", Kind: "camera", Healthy: true}}})
	service.video = inferenceTestVideo{}
	return service, &inferenceTestFactory{sessions: make(chan *inferenceTestSession, 8)}, &inferenceTestSender{requests: make(chan DetectionNotification, 8)}
}

func startDocumentTestService(t *testing.T, service *DataService, factory *inferenceTestFactory, sender CampaignNotificationSender) {
	t.Helper()
	stop := service.StartCampaignInference(context.Background(), factory, sender)
	t.Cleanup(func() {
		stop()
		for _, key := range service.manager.ActiveEpisodeKeys() {
			_, _ = service.stopCapture(context.Background(), key)
		}
	})
}

func saveDocument(t *testing.T, service *DataService, contents string) data.StoredDocument {
	t.Helper()
	doc, err := data.ParseDocument([]byte(contents))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := service.manager.SaveDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	if service.inference != nil {
		service.inference.wakeNow()
	}
	return stored
}

func personResult(input inference.Input) inference.Result {
	return inference.Result{Type: "prediction", SourceID: input.SourceID, Generation: input.Generation, Detections: []inference.Detection{{Label: "person", Score: .99, Box: [4]float64{1, 2, 3, 4}}}}
}

func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const testClipsCollection = `kind: collection
version: 1
name: clips
sources:
  - camera: "*"
capture:
  buffer: 0s
  drain: 0s
  after_trigger: 1s
  triggers: [{event: front-door}]
upload: {when: manual}
export: {annotation: cvat}
`

func TestInferenceDocumentRunsAndTriggersItsCollection(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	startDocumentTestService(t, service, factory, sender)
	if _, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: []byte(testClipsCollection)}); err != nil {
		t.Fatal(err)
	}
	other := strings.ReplaceAll(strings.Replace(testClipsCollection, "name: clips", "name: other", 1), "event: front-door", "event: something-else")
	if _, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: []byte(other)}); err != nil {
		t.Fatal(err)
	}
	stored := saveDocument(t, service, testInferenceDocumentYAML)
	session := receiveInference(t, factory.sessions)
	input := receiveInference(t, session.inputs)
	eventually(t, "the inference job to run", func() bool {
		return service.inference.snapshot(inferenceJobKey("front-door"), stored.Revision).State == "running"
	})
	session.results <- personResult(input)
	eventually(t, "the clips collection to trigger", func() bool {
		return slices.Contains(service.manager.ActiveEpisodeKeys(), "clips")
	})
	if slices.Contains(service.manager.ActiveEpisodeKeys(), "other") {
		t.Fatal("a collection triggered by another event started")
	}
	if got := service.collectionsTriggeredBy("front-door"); len(got) != 1 || !got["clips"] {
		t.Fatalf("audience = %v", got)
	}
}

func TestInferenceDocumentAndCampaignWithOneNameBothRun(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	startDocumentTestService(t, service, factory, sender)
	if _, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: inferenceTestYAML(t)}); err != nil {
		t.Fatal(err)
	}
	first := receiveInference(t, factory.sessions)
	saveDocument(t, service, strings.Replace(testInferenceDocumentYAML, "name: front-door", "name: people-all-cameras", 1))
	second := receiveInference(t, factory.sessions)
	time.Sleep(300 * time.Millisecond)
	for _, session := range []*inferenceTestSession{first, second} {
		select {
		case <-session.closed:
			t.Fatal("one job retired the other")
		default:
		}
	}
	if !service.inference.inferenceActive(campaignJobKey("people-all-cameras")) {
		t.Fatal("the campaign's job is not active")
	}
}

func TestDisabledInferenceDocumentDoesNotRun(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	startDocumentTestService(t, service, factory, sender)
	saveDocument(t, service, testInferenceDocumentYAML+"enabled: false\n")
	select {
	case <-factory.sessions:
		t.Fatal("a disabled inference document started a model")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestRemovedInferenceDocumentStopsOnReconcileNow(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	startDocumentTestService(t, service, factory, sender)
	saveDocument(t, service, testInferenceDocumentYAML)
	session := receiveInference(t, factory.sessions)
	if err := service.manager.RemoveDocument(data.KindInference, "front-door"); err != nil {
		t.Fatal(err)
	}
	service.inference.reconcileNow()
	select {
	case <-session.closed:
	default:
		t.Fatal("reconcileNow returned before the job stopped")
	}
}

func TestInferenceDocumentModelInputsUseItsAppIDAndAudience(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	startDocumentTestService(t, service, factory, sender)
	for _, yaml := range []string{testClipsCollection, strings.ReplaceAll(strings.Replace(testClipsCollection, "name: clips", "name: other", 1), "event: front-door", "event: something-else")} {
		if _, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: []byte(yaml)}); err != nil {
			t.Fatal(err)
		}
	}
	// The test camera yields one sample, so both episodes are open before the
	// document starts: one in the document's audience, one unrelated.
	for _, name := range []string{"clips", "other"} {
		if _, err := service.manager.Start(data.StartOptions{Sources: []string{"applications"}, Trigger: data.EpisodeTrigger{Reason: "manual", CampaignName: name}}); err != nil {
			t.Fatal(err)
		}
	}
	saveDocument(t, service, testInferenceDocumentYAML)
	session := receiveInference(t, factory.sessions)
	receiveInference(t, session.inputs)
	ledgers := func(key string) string {
		session, ok := service.manager.ActiveSession(key)
		if !ok {
			return ""
		}
		b, _ := os.ReadFile(filepath.Join(session.Directory, data.ModelInputLedgerFile))
		return string(b)
	}
	eventually(t, "a model input in the clips ledger", func() bool {
		return strings.Contains(ledgers("clips"), `"app_id":"sh.wendy.inference.front-door"`)
	})
	if strings.Contains(ledgers("clips"), "sh.wendy.campaign.") {
		t.Fatal("a document's model input carried a campaign app id")
	}
	if ledgers("other") != "" {
		t.Fatalf("an unrelated episode received model inputs: %s", ledgers("other"))
	}
}

func TestOwnedInferenceWritesNothingOnceItsOwnersLeaseLapsed(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	clock := newFakeClock()
	service.leases.now = clock.Now
	startDocumentTestService(t, service, factory, sender)
	if _, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: []byte(testClipsCollection)}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, service, watchYAML("chat-1", "front-door", "lease: 15s\n"))
	stored := mustApply(t, service, ownedInference("front-door", "watch/chat-1"))
	session := receiveInference(t, factory.sessions)
	input := receiveInference(t, session.inputs)
	eventually(t, "the inference job to run", func() bool {
		return service.inference.snapshot(inferenceJobKey("front-door"), stored.GetRevision()).State == "running"
	})

	clock.Add(16 * time.Second) // the lease lapsed; the 5 s expiry pass has not removed the watch
	session.results <- personResult(input)
	// The job handles results in order, so once it has handled the marker it
	// has finished the frame before it.
	session.results <- inference.Result{Type: "source_error", SourceID: input.SourceID, Generation: input.Generation, Error: "marker"}
	eventually(t, "the job to pass the lapsed frame", func() bool {
		return service.inference.snapshot(inferenceJobKey("front-door"), stored.GetRevision()).Sources[input.SourceID] == "marker"
	})
	if slices.Contains(service.manager.ActiveEpisodeKeys(), "clips") {
		t.Fatal("a lapsed owner's inference wrote an event that triggered capture")
	}
	if entries := journal(t, service, "chat-1"); len(entries) != 0 {
		t.Fatalf("a lapsed owner's inference notified its watch: %+v", entries)
	}

	// Control: the same frame is processed once the lease is live again.
	service.leases.extend("chat-1", time.Minute)
	session.results <- personResult(input)
	eventually(t, "the live lease's event to trigger capture", func() bool {
		return slices.Contains(service.manager.ActiveEpisodeKeys(), "clips")
	})
	if entries := journal(t, service, "chat-1"); len(entries) != 1 {
		t.Fatalf("journal = %+v; want only the live frame's entry", entries)
	}
}
