package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type inferenceTestAdapter struct {
	mu        sync.Mutex
	sources   []data.Source
	failStart bool
}

func (a *inferenceTestAdapter) Discover(context.Context) []data.Source {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]data.Source(nil), a.sources...)
}
func (a *inferenceTestAdapter) Start(context.Context, data.CaptureSession, []data.Source) (runningDataCapture, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failStart {
		return nil, errors.New("camera recorder failed")
	}
	return nil, nil
}

type inferenceTestVideo struct{}

func (inferenceTestVideo) SubscribeSensor(context.Context, string) (sensorSubscription, error) {
	return &inferenceTestSubscription{}, nil
}

type inferenceTestSubscription struct{ sent bool }

func (s *inferenceTestSubscription) Next(ctx context.Context) (SensorSample, error) {
	if !s.sent {
		s.sent = true
		return SensorSample{SampleID: 1, Payload: []byte("encoded frame"), Encoding: "h264"}, nil
	}
	<-ctx.Done()
	return SensorSample{}, ctx.Err()
}
func (*inferenceTestSubscription) Close() {}

type inferenceTestSession struct {
	mu      sync.Mutex
	results chan inference.Result
	inputs  chan inference.Input
	closed  chan struct{}
	stopped bool
}

func (s *inferenceTestSession) Send(input inference.Input) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return errors.New("closed")
	}
	if !input.End {
		s.inputs <- input
	}
	return nil
}
func (s *inferenceTestSession) Results() <-chan inference.Result { return s.results }
func (s *inferenceTestSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.closed)
	}
	return nil
}

type inferenceTestFactory struct{ sessions chan *inferenceTestSession }

func (f *inferenceTestFactory) Start(context.Context, data.CampaignInference) (inference.Session, error) {
	s := &inferenceTestSession{results: make(chan inference.Result, 16), inputs: make(chan inference.Input, 16), closed: make(chan struct{})}
	f.sessions <- s
	return s, nil
}

type inferenceTestSender struct {
	requests chan DetectionNotification
}

func (s *inferenceTestSender) Send(_ context.Context, _ string, request DetectionNotification) error {
	s.requests <- request
	return nil
}

func inferenceTestYAML(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../../../Examples/WendyDataPeople/campaign.yaml")
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), "buffer: 10s", "buffer: 0s\n  drain: 0s"))
	raw = []byte(strings.ReplaceAll(string(raw), "  on: episode_committed", "  on: detection\n  webhook: https://notifications.example/detections"))
	return raw
}

func receiveInference[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for inference")
		var zero T
		return zero
	}
}

func newInferenceTestService(t *testing.T, restore bool) (*DataService, *inferenceTestFactory, *inferenceTestSender, *inferenceTestAdapter) {
	t.Helper()
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	adapter := &inferenceTestAdapter{sources: []data.Source{
		{ID: "v4l2:/dev/video0", Kind: "camera", Healthy: true},
		{ID: "ipcamera:1000000", Kind: "camera", Healthy: true},
		{ID: "ipcamera:1000001", Kind: "camera", Healthy: false, Detail: "credentials required"},
	}}
	service.addAdapter(adapter)
	service.video = inferenceTestVideo{}
	factory := &inferenceTestFactory{sessions: make(chan *inferenceTestSession, 8)}
	sender := &inferenceTestSender{requests: make(chan DetectionNotification, 8)}
	if restore {
		if _, err := manager.DeployCampaign(inferenceTestYAML(t)); err != nil {
			t.Fatal(err)
		}
	}
	stop := service.StartCampaignInference(context.Background(), factory, sender)
	t.Cleanup(func() {
		stop()
		for _, key := range manager.ActiveEpisodeKeys() {
			_, _ = service.stopCapture(context.Background(), key)
		}
	})
	if !restore {
		if _, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: inferenceTestYAML(t)}); err != nil {
			t.Fatal(err)
		}
	}
	return service, factory, sender, adapter
}

func TestAgentInferenceAllCamerasRecordAndNotify(t *testing.T) {
	service, factory, sender, _ := newInferenceTestService(t, false)
	session := receiveInference(t, factory.sessions)
	first, second := receiveInference(t, session.inputs), receiveInference(t, session.inputs)
	if first.SourceID == second.SourceID {
		t.Fatal("only one camera was subscribed")
	}
	result := func(input inference.Input) inference.Result {
		return inference.Result{Type: "prediction", SourceID: input.SourceID, Generation: input.Generation, Detections: []inference.Detection{{Label: "person", Score: .99, Box: [4]float64{1, 2, 3, 4}}}}
	}
	session.results <- result(first)
	notification1 := receiveInference(t, sender.requests)
	session.results <- result(second)
	notification2 := receiveInference(t, sender.requests)
	if notification1.ID == notification2.ID {
		t.Fatal("different cameras shared a notification ID")
	}

	episodes := service.manager.ActiveEpisodeKeys()
	if len(episodes) != 1 {
		t.Fatalf("two detections should share one episode: %v", episodes)
	}
	current := service.manager.Status()
	if current == nil || len(current.Sources) != 3 {
		t.Fatalf("episode must record both cameras and applications: %+v", current)
	}
	if current.Trigger.Notify != nil {
		t.Fatal("immediate notification incorrectly forwarded as ingest notification intent")
	}
	// Repeated positive frames must not retrigger notification.
	session.results <- result(first)
	select {
	case <-sender.requests:
		t.Fatal("standing person retriggered notification")
	case <-time.After(50 * time.Millisecond):
	}
	inspection, err := service.CampaignInspect(context.Background(), &agentpbv2.DataCampaignInspectRequest{Name: "people-all-cameras"})
	if err != nil {
		t.Fatal(err)
	}
	var plan data.Campaign
	if err := json.Unmarshal(inspection.PlanJson, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.InferenceStatus.State != "running" || !strings.Contains(plan.InferenceStatus.Sources["ipcamera:1000001"], "credentials") {
		t.Fatalf("missing runtime health: %+v", plan.InferenceStatus)
	}
}

func TestAgentInferenceRestoresAndDiscoversNewCamera(t *testing.T) {
	_, factory, _, adapter := newInferenceTestService(t, true)
	session := receiveInference(t, factory.sessions)
	receiveInference(t, session.inputs)
	receiveInference(t, session.inputs)
	adapter.mu.Lock()
	adapter.sources[2].Healthy = true
	adapter.mu.Unlock()
	added := receiveInference(t, session.inputs)
	if added.SourceID != "ipcamera:1000001" {
		t.Fatalf("recovered camera not subscribed: %+v", added)
	}
}

func TestAgentInferenceRestartsExitedWorker(t *testing.T) {
	_, factory, _, _ := newInferenceTestService(t, true)
	first := receiveInference(t, factory.sessions)
	close(first.results)
	second := receiveInference(t, factory.sessions)
	receiveInference(t, first.closed)
	if first == second {
		t.Fatal("exited model worker was reused")
	}
	one, two := receiveInference(t, second.inputs), receiveInference(t, second.inputs)
	if one.SourceID == two.SourceID {
		t.Fatal("replacement worker lost a camera")
	}
}

func TestAgentInferenceRedeployStopsOldRevision(t *testing.T) {
	service, factory, sender, _ := newInferenceTestService(t, false)
	first := receiveInference(t, factory.sessions)
	input := receiveInference(t, first.inputs)
	old, err := service.manager.Campaign("people-all-cameras")
	if err != nil {
		t.Fatal(err)
	}
	updated := []byte(strings.ReplaceAll(string(inferenceTestYAML(t)), "threshold: 0.9", "threshold: 0.8"))
	if _, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: updated}); err != nil {
		t.Fatal(err)
	}
	second := receiveInference(t, factory.sessions)
	receiveInference(t, first.closed)
	accepted, err := service.triggerInference(context.Background(), old, data.ApplicationRecord{Version: 1, Type: "event", Name: "person_detected"})
	if accepted || err != nil {
		t.Fatalf("retired revision triggered: %v, %v", accepted, err)
	}
	// Retired stream generations cannot be attributed to a new worker.
	second.results <- inference.Result{Type: "prediction", SourceID: input.SourceID, Generation: 999999, Detections: []inference.Detection{{Label: "person", Score: 1}}}
	disabled := []byte(strings.ReplaceAll(string(updated), "enabled: true", "enabled: false"))
	if _, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: disabled}); err != nil {
		t.Fatal(err)
	}
	receiveInference(t, second.closed)
	select {
	case <-sender.requests:
		t.Fatal("stale result sent notification")
	default:
	}
	if len(service.manager.ActiveEpisodeKeys()) != 0 {
		t.Fatal("stale result started recording")
	}
}

func TestAgentInferencePresenceAndFiltering(t *testing.T) {
	campaign, err := data.ParseCampaign(inferenceTestYAML(t))
	if err != nil {
		t.Fatal(err)
	}
	config := campaign.Inference
	now := time.Now()
	p := &inferencePresence{}
	if p.observe(false, now, config) || !p.observe(true, now.Add(time.Second), config) || p.observe(true, now.Add(40*time.Second), config) {
		t.Fatal("presence edge incorrect")
	}
	p.observe(false, now.Add(41*time.Second), config)
	p.observe(false, now.Add(100*time.Second), config) // outage is not empty time
	if p.observe(true, now.Add(101*time.Second), config) {
		t.Fatal("outage rearmed detection")
	}
	p.observe(false, now.Add(102*time.Second), config)
	p.observe(false, now.Add(107*time.Second), config)
	if !p.observe(true, now.Add(108*time.Second), config) {
		t.Fatal("empty observations did not rearm")
	}
	filtered, err := validateDetections(config, []inference.Detection{{Label: "cat", Score: 1}, {Label: "person", Score: .5}, {Label: "person", Score: .99}})
	if err != nil || len(filtered) != 1 {
		t.Fatalf("incorrect label/threshold filtering: %v, %v", filtered, err)
	}
}

func TestAgentInferenceDeploymentRequiresRuntime(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	_, err = service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: inferenceTestYAML(t)})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unavailable runtime deployed silently: %v", err)
	}
}

func TestAgentInferenceRecordingFailureStillNotifies(t *testing.T) {
	service, factory, sender, adapter := newInferenceTestService(t, false)
	adapter.mu.Lock()
	adapter.failStart = true
	adapter.mu.Unlock()
	session := receiveInference(t, factory.sessions)
	input := receiveInference(t, session.inputs)
	session.results <- inference.Result{Type: "prediction", SourceID: input.SourceID, Generation: input.Generation, Detections: []inference.Detection{{Label: "person", Score: .99}}}
	request := receiveInference(t, sender.requests)
	if request.SourceID != input.SourceID {
		t.Fatal("notification lost camera identity")
	}
	if len(service.manager.ActiveEpisodeKeys()) != 0 {
		t.Fatal("failed recorder left an active episode")
	}
}

func TestAgentInferenceCloudCommitNotificationStaysInManifest(t *testing.T) {
	service, factory, sender, _ := newInferenceTestService(t, false)
	receiveInference(t, factory.sessions)
	raw, err := os.ReadFile("../../../../Examples/WendyDataPeople/campaign.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: raw}); err != nil {
		t.Fatal(err)
	}
	session := receiveInference(t, factory.sessions)
	input := receiveInference(t, session.inputs)
	session.results <- inference.Result{Type: "prediction", SourceID: input.SourceID, Generation: input.Generation, Detections: []inference.Detection{{Label: "person", Score: .99}}}
	deadline := time.Now().Add(2 * time.Second)
	for service.manager.Status() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	manifest := service.manager.Status()
	if manifest == nil || manifest.Trigger.Notify == nil || manifest.Trigger.Notify.On != data.NotifyOnEpisodeCommitted {
		t.Fatalf("cloud ingest notification intent missing: %+v", manifest)
	}
	select {
	case <-sender.requests:
		t.Fatal("cloud-commit plan sent an immediate webhook")
	default:
	}
}

type retryInferenceSender struct{ ids chan string }

func (s *retryInferenceSender) Send(_ context.Context, _ string, request DetectionNotification) error {
	s.ids <- request.ID
	if len(s.ids) == 1 {
		return errors.New("connection interrupted")
	}
	return nil
}

func TestAgentInferenceNotificationRetryKeepsIdentity(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sender := &retryInferenceSender{ids: make(chan string, 4)}
	campaign, err := data.ParseCampaign(inferenceTestYAML(t))
	if err != nil {
		t.Fatal(err)
	}
	job := &campaignInferenceJob{owner: &campaignInferenceManager{service: NewDataService(manager), sender: sender}, campaign: campaign}
	request := detectionNotification(campaign, "v4l2:/dev/video0", 1)
	queue := make(chan DetectionNotification, 1)
	queue <- request
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); job.notifications(ctx, queue) }()
	deadline := time.Now().Add(3 * time.Second)
	for len(sender.ids) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	receiveInference(t, done)
	if len(sender.ids) != 2 {
		t.Fatal("notification was not retried")
	}
	if <-sender.ids != request.ID || <-sender.ids != request.ID {
		t.Fatal("retry changed notification UUID")
	}
	if job.status.NotificationError != "" {
		t.Fatalf("successful retry must clear notification error: %s", job.status.NotificationError)
	}
}
