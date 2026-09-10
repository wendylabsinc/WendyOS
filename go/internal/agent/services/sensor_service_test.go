package services

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	appspbv1 "github.com/wendylabsinc/wendy/go/proto/gen/appspb/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeSensorProvider serves a fixed script of samples for one source. Once the
// script is exhausted it blocks until the stream is cancelled, mirroring a live
// producer that simply has no new sample yet — a producer that STOPS is a
// reportable failure, and tests that want that set stopAfterScript.
type fakeSensorProvider struct {
	sourceID string
	samples  []SensorSample
	// stopAfterScript makes the subscription report a stopped producer once the
	// script runs out instead of waiting for cancellation.
	stopAfterScript bool
	// gate releases one scripted sample per token. It makes a test deterministic
	// against the service's own drop policy: the fan-in queue deliberately drops
	// when a model is not keeping up, so a producer that dumps its whole script
	// at once would race the sender.
	gate chan struct{}
	// subscribed counts SubscribeSensor calls, so a test can assert that the
	// provider (and therefore the shared producer) was joined exactly once.
	subscribed atomic.Int32
	closed     atomic.Int32
}

func (p *fakeSensorProvider) SupportsSensorSource(sourceID string) bool {
	return sourceID == p.sourceID
}

func (p *fakeSensorProvider) SubscribeSensor(context.Context, string) (sensorSubscription, error) {
	p.subscribed.Add(1)
	return &fakeSensorSubscription{provider: p, remaining: p.samples}, nil
}

type fakeSensorSubscription struct {
	provider  *fakeSensorProvider
	remaining []SensorSample
}

func (s *fakeSensorSubscription) Next(ctx context.Context) (SensorSample, error) {
	if s.provider.gate != nil {
		select {
		case <-s.provider.gate:
		case <-ctx.Done():
			return SensorSample{}, ctx.Err()
		}
	}
	if len(s.remaining) == 0 {
		if s.provider.stopAfterScript {
			return SensorSample{}, errSensorProducerStopped
		}
		<-ctx.Done()
		return SensorSample{}, ctx.Err()
	}
	sample := s.remaining[0]
	s.remaining = s.remaining[1:]
	return sample, nil
}

func (s *fakeSensorSubscription) Close() { s.provider.closed.Add(1) }

func sensorScript(sourceID string, count int) []SensorSample {
	out := make([]SensorSample, 0, count)
	for i := 1; i <= count; i++ {
		out = append(out, SensorSample{
			SourceID: sourceID, SampleID: uint64(i), BootNanos: int64(i) * int64(time.Millisecond),
			UncertaintyNanos: 1000, Payload: []byte{byte(i)}, Encoding: "h264", SelfContained: true,
		})
	}
	return out
}

// subscribeAll runs one Subscribe call and returns once `want` samples have been
// delivered, cancelling the stream the way a real client disconnecting does. The
// provider is driven one sample at a time so the assertion is on the contract
// and not on a race with the fan-in queue.
func subscribeAll(t *testing.T, service *SensorService, provider *fakeSensorProvider, req *appspbv1.SensorSubscribeRequest, want int) []*appspbv1.SensorSample {
	t.Helper()
	provider.gate = make(chan struct{}, 1)
	provider.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &fakeServerStream[appspbv1.SensorSample]{ctx: ctx}
	stream.onSend = func(count int) error {
		if count >= want {
			// Cancelled after the send returns, so the sample is already in the
			// model's hands and the tee that follows it still runs.
			cancel()
			return nil
		}
		provider.gate <- struct{}{}
		return nil
	}
	if err := service.Subscribe(req, stream); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	return stream.sent
}

// TestSubscribeStreamsIdentifiedSamples is the core of the subscribe contract:
// a model receives the source identity, a per-source sample identifier, and the
// canonical boot-clock timestamp alongside the payload.
func TestSubscribeStreamsIdentifiedSamples(t *testing.T) {
	provider := &fakeSensorProvider{sourceID: "v4l2:/dev/video0", samples: sensorScript("v4l2:/dev/video0", 3)}
	service := NewSensorService("sh.wendy.test", nil)
	service.AddProvider(provider)

	sent := subscribeAll(t, service, provider, &appspbv1.SensorSubscribeRequest{SourceIds: []string{"v4l2:/dev/video0"}}, 3)
	if len(sent) != 3 {
		t.Fatalf("delivered %d samples, want 3", len(sent))
	}
	for i, sample := range sent {
		if sample.GetSourceId() != "v4l2:/dev/video0" {
			t.Errorf("sample %d source = %q", i, sample.GetSourceId())
		}
		if sample.GetSampleId() != uint64(i+1) {
			t.Errorf("sample %d id = %d, want %d", i, sample.GetSampleId(), i+1)
		}
		if sample.GetBoottimeNanos() == 0 || sample.GetTimestampUncertaintyNanos() == 0 {
			t.Errorf("sample %d carries no bracketed boot time: %+v", i, sample)
		}
		if sample.GetEncoding() != "h264" || !sample.GetPayloadSelfContained() {
			t.Errorf("sample %d payload description = %q/%v", i, sample.GetEncoding(), sample.GetPayloadSelfContained())
		}
	}
	if provider.subscribed.Load() != 1 {
		t.Errorf("provider joined %d times, want 1", provider.subscribed.Load())
	}
	if provider.closed.Load() != 1 {
		t.Errorf("subscription closed %d times, want 1", provider.closed.Load())
	}
}

func TestSubscribeRejectsMalformedRequests(t *testing.T) {
	service := NewSensorService("sh.wendy.test", nil)
	service.AddProvider(&fakeSensorProvider{sourceID: "a"})
	for name, req := range map[string]*appspbv1.SensorSubscribeRequest{
		"no sources": {},
		"empty id":   {SourceIds: []string{""}},
		"unknown id": {SourceIds: []string{"nope"}},
		"too many":   {SourceIds: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}},
	} {
		stream := &fakeServerStream[appspbv1.SensorSample]{ctx: context.Background()}
		err := service.Subscribe(req, stream)
		if err == nil {
			t.Errorf("%s: Subscribe accepted the request", name)
			continue
		}
		if code := status.Code(err); code != codes.InvalidArgument && code != codes.NotFound {
			t.Errorf("%s: code = %s", name, code)
		}
	}
}

// startEpisodeWithSource starts an episode that selects one adapter-provided
// source, returning the manager and the episode directory.
func startEpisodeWithSource(t *testing.T, source data.Source, capture *data.SourceCapture, selected bool) (*data.Manager, string) {
	t.Helper()
	root := t.TempDir()
	manager, err := data.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSourceProvider(func(context.Context) []data.Source { return []data.Source{source} })
	sources := []string{"applications"}
	captures := map[string]*data.SourceCapture{}
	if selected {
		sources = append(sources, source.ID)
		if capture != nil {
			captures[source.ID] = capture
		}
	}
	if _, err := manager.Start(data.StartOptions{Name: "tee", Sources: sources, SourceCaptures: captures}); err != nil {
		t.Fatal(err)
	}
	session, ok := manager.ActiveSession(data.AdHocEpisodeKey)
	if !ok {
		t.Fatal("no active session")
	}
	return manager, session.Directory
}

func readLedger(t *testing.T, dir string) []data.ModelInput {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, data.ModelInputLedgerFile))
	if err != nil {
		t.Fatalf("opening the model-input ledger: %v", err)
	}
	defer f.Close()
	var out []data.ModelInput
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var input data.ModelInput
		if err := json.Unmarshal(scanner.Bytes(), &input); err != nil {
			t.Fatalf("ledger line is not JSON: %v", err)
		}
		out = append(out, input)
	}
	return out
}

// TestSubscribeTeesDeliveredSamplesIntoActiveEpisode is the defect this change
// exists to fix: a sample handed to a model must appear in the active episode
// under the very identifier the model was given.
func TestSubscribeTeesDeliveredSamplesIntoActiveEpisode(t *testing.T) {
	source := data.Source{ID: "v4l2:/dev/video0", Kind: "camera", ClockDomain: "V4L2_BUFFER_TIMESTAMP", Healthy: true}
	manager, dir := startEpisodeWithSource(t, source, nil, true)
	service := NewSensorService("sh.wendy.test", manager)
	provider := &fakeSensorProvider{sourceID: source.ID, samples: sensorScript(source.ID, 4)}
	service.AddProvider(provider)

	sent := subscribeAll(t, service, provider, &appspbv1.SensorSubscribeRequest{SourceIds: []string{source.ID}, Model: "yolov8n"}, 4)
	if len(sent) != 4 {
		t.Fatalf("delivered %d samples", len(sent))
	}
	ledger := readLedger(t, dir)
	if len(ledger) != len(sent) {
		t.Fatalf("ledger holds %d entries for %d delivered samples", len(ledger), len(sent))
	}
	for i, entry := range ledger {
		if entry.SourceID != sent[i].GetSourceId() || entry.SampleID != sent[i].GetSampleId() {
			t.Errorf("ledger entry %d = %s#%d, delivered %s#%d", i, entry.SourceID, entry.SampleID, sent[i].GetSourceId(), sent[i].GetSampleId())
		}
		if entry.Model != "yolov8n" || entry.AppID != "sh.wendy.test" {
			t.Errorf("ledger entry %d is not attributed: %+v", i, entry)
		}
		if entry.PayloadBytes != len(sent[i].GetPayload()) {
			t.Errorf("ledger entry %d payload_bytes = %d, delivered %d", i, entry.PayloadBytes, len(sent[i].GetPayload()))
		}
	}

	manifest, err := manager.Stop(data.AdHocEpisodeKey)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ModelIO.SamplesDelivered != 4 || manifest.ModelIO.InputLedger != data.ModelInputLedgerFile {
		t.Fatalf("manifest model_io = %+v", manifest.ModelIO)
	}
	var stats *data.SourceModelInputs
	for _, s := range manifest.Sources {
		if s.Source.ID == source.ID {
			stats = s.ModelInputs
		}
	}
	if stats == nil {
		t.Fatal("captured source carries no model-input accounting")
	}
	if stats.Delivered != 4 || stats.FirstSampleID != 1 || stats.LastSampleID != 4 {
		t.Fatalf("source accounting = %+v", stats)
	}
	if stats.PayloadRetention != data.RetentionCapturePolicy {
		t.Errorf("retention = %q, want %q for an uncapped continuous capture", stats.PayloadRetention, data.RetentionCapturePolicy)
	}
}

// TestTeeReportsPolicySubsetHonestly covers the requested-versus-kept case: a
// snapshot policy keeps far less than the model consumed, and the episode must
// say so rather than implying it holds every frame.
func TestTeeReportsPolicySubsetHonestly(t *testing.T) {
	source := data.Source{ID: "v4l2:/dev/video0", Kind: "camera", Healthy: true}
	capture := &data.SourceCapture{Mode: "snapshot", Interval: "30s"}
	manager, _ := startEpisodeWithSource(t, source, capture, true)
	service := NewSensorService("sh.wendy.test", manager)
	provider := &fakeSensorProvider{sourceID: source.ID, samples: sensorScript(source.ID, 2)}
	service.AddProvider(provider)
	subscribeAll(t, service, provider, &appspbv1.SensorSubscribeRequest{SourceIds: []string{source.ID}}, 2)

	manifest, err := manager.Stop(data.AdHocEpisodeKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range manifest.Sources {
		if s.Source.ID != source.ID {
			continue
		}
		if s.ModelInputs == nil || s.ModelInputs.PayloadRetention != data.RetentionPolicySubset {
			t.Fatalf("snapshot capture reported retention %+v, want %q", s.ModelInputs, data.RetentionPolicySubset)
		}
		if s.ModelInputs.Note == "" {
			t.Error("policy-subset retention carries no explanation")
		}
		return
	}
	t.Fatal("source missing from the manifest")
}

// TestTeeRecordsSourceTheEpisodeDoesNotCapture keeps the episode from implying
// it holds payload bytes for a source it never captured.
func TestTeeRecordsSourceTheEpisodeDoesNotCapture(t *testing.T) {
	source := data.Source{ID: "v4l2:/dev/video1", Kind: "camera", Healthy: true}
	manager, dir := startEpisodeWithSource(t, source, nil, false)
	service := NewSensorService("sh.wendy.test", manager)
	provider := &fakeSensorProvider{sourceID: source.ID, samples: sensorScript(source.ID, 2)}
	service.AddProvider(provider)
	subscribeAll(t, service, provider, &appspbv1.SensorSubscribeRequest{SourceIds: []string{source.ID}}, 2)

	if entries := readLedger(t, dir); len(entries) != 2 {
		t.Fatalf("ledger holds %d entries, want 2", len(entries))
	}
	manifest, err := manager.Stop(data.AdHocEpisodeKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.ModelIO.Uncaptured) != 1 {
		t.Fatalf("uncaptured accounting = %+v", manifest.ModelIO.Uncaptured)
	}
	uncaptured := manifest.ModelIO.Uncaptured[0]
	if uncaptured.SourceID != source.ID || uncaptured.PayloadRetention != data.RetentionNotCaptured || uncaptured.Delivered != 2 {
		t.Fatalf("uncaptured entry = %+v", uncaptured)
	}
}

// TestPredictionInputCorrelationReachesTheManifest is the deliverable: an
// episode must let a consumer pair outcomes with the inputs that produced them,
// and must count the outcomes it cannot pair.
func TestPredictionInputCorrelationReachesTheManifest(t *testing.T) {
	source := data.Source{ID: "v4l2:/dev/video0", Kind: "camera", Healthy: true}
	manager, dir := startEpisodeWithSource(t, source, nil, true)
	service := NewSensorService("sh.wendy.test", manager)
	provider := &fakeSensorProvider{sourceID: source.ID, samples: sensorScript(source.ID, 3)}
	service.AddProvider(provider)
	subscribeAll(t, service, provider, &appspbv1.SensorSubscribeRequest{SourceIds: []string{source.ID}}, 3)

	bootID := data.BootID()
	_, now, _, err := data.CaptureReceipt()
	if err != nil {
		t.Fatal(err)
	}
	record := func(inputs []data.SampleRef) data.ApplicationRecord {
		return data.ApplicationRecord{Version: 1, Type: "prediction", Model: "yolov8n", Inputs: inputs, ClientBootNanos: now, ClientBootID: bootID}
	}
	// One prediction bound to a delivered sample, one bound to a sample the
	// episode never delivered, one with no binding at all.
	for _, rec := range []data.ApplicationRecord{
		record([]data.SampleRef{{SourceID: source.ID, SampleID: 2}}),
		record([]data.SampleRef{{SourceID: source.ID, SampleID: 9999}}),
		record(nil),
	} {
		if err := validateApplicationRecord(rec); err != nil {
			t.Fatalf("record rejected: %v", err)
		}
		if _, err := manager.RecordApplication("sh.wendy.test", rec); err != nil {
			t.Fatal(err)
		}
	}

	manifest, err := manager.Stop(data.AdHocEpisodeKey)
	if err != nil {
		t.Fatal(err)
	}
	io := manifest.ModelIO
	if io.Predictions != 3 || io.PredictionsWithInputs != 2 || io.ReferencesOutsideDelivered != 1 {
		t.Fatalf("model_io = %+v", io)
	}
	if io.OutcomeLog != "events.jsonl" || io.PayloadLocator == "" {
		t.Fatalf("manifest does not describe the reconstruction: %+v", io)
	}
	// app_id belongs in the join. Sources are shared, so two apps can be
	// delivered the same (source_id, sample_id); a join without app_id would
	// pair one app's prediction with another app's input.
	if got, want := strings.Join(io.JoinKeys, ","), "app_id,source_id,sample_id"; got != want {
		t.Fatalf("join_keys = %q, want %q", got, want)
	}
	// The reference itself must survive into the stored outcome, or the
	// manifest counters would be the only trace of the correlation.
	events, err := os.ReadFile(filepath.Join(strings.TrimSuffix(dir, ".partial"), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), `"inputs":[{"source_id":"v4l2:/dev/video0","sample_id":2}]`) {
		t.Fatalf("stored outcome lost its input reference: %s", events)
	}
}

// TestEventRecordCannotClaimInputs keeps the kind registry authoritative about
// which records may bind to samples.
func TestEventRecordCannotClaimInputs(t *testing.T) {
	rec := data.ApplicationRecord{Version: 1, Type: "event", Name: "person_detected", Inputs: []data.SampleRef{{SourceID: "a", SampleID: 1}}}
	if err := validateApplicationRecord(rec); err == nil {
		t.Fatal("an event record was allowed to reference input samples")
	}
}

func TestPredictionInputRefsAreBounded(t *testing.T) {
	refs := make([]data.SampleRef, 40)
	for i := range refs {
		refs[i] = data.SampleRef{SourceID: "a", SampleID: uint64(i)}
	}
	if err := validateApplicationRecord(data.ApplicationRecord{Version: 1, Type: "prediction", Model: "m", Inputs: refs}); err == nil {
		t.Fatal("an unbounded input reference list was accepted")
	}
	if err := validateApplicationRecord(data.ApplicationRecord{Version: 1, Type: "prediction", Model: "m", Inputs: []data.SampleRef{{SampleID: 1}}}); err == nil {
		t.Fatal("an input reference with no source was accepted")
	}
}

// TestSensorSocketAuthorizesOnlySensorService guards the narrowness of the
// grant: the socket must refuse any method outside SensorService even if
// something else is ever registered on that server.
func TestSensorSocketAuthorizesOnlySensorService(t *testing.T) {
	if err := authorizeSensorMethod(appspbv1.SensorService_Subscribe_FullMethodName); err != nil {
		t.Fatalf("Subscribe refused: %v", err)
	}
	if err := authorizeSensorMethod(appspbv1.SensorService_Sources_FullMethodName); err != nil {
		t.Fatalf("Sources refused: %v", err)
	}
	for _, method := range []string{
		agentpbv2.DataService_Start_FullMethodName,
		agentpbv2.DataService_CampaignDeploy_FullMethodName,
		agentpbv2.DataService_Download_FullMethodName,
		"/wendy.agent.apps.v1.SensorServiceEvil/Subscribe",
	} {
		if err := authorizeSensorMethod(method); status.Code(err) != codes.PermissionDenied {
			t.Errorf("method %s was authorized on the sensor socket (err %v)", method, err)
		}
	}
}

// TestSensorSourcesMarksUnsubscribableSources keeps a source that no producer
// can multiplex visible and explained rather than silently missing.
func TestSensorSourcesMarksUnsubscribableSources(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSourceProvider(func(context.Context) []data.Source {
		return []data.Source{{ID: "v4l2:/dev/video0", Kind: "camera", Healthy: true}, {ID: "audio:1", Kind: "audio", Healthy: true}}
	})
	service := NewSensorService("sh.wendy.test", manager)
	service.AddProvider(&fakeSensorProvider{sourceID: "v4l2:/dev/video0"})
	response, err := service.Sources(context.Background(), &appspbv1.SensorSourcesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]*appspbv1.SensorSource{}
	for _, source := range response.GetSources() {
		found[source.GetId()] = source
	}
	if camera := found["v4l2:/dev/video0"]; camera == nil || !camera.GetSubscribable() {
		t.Fatalf("camera source = %+v, want subscribable", camera)
	}
	audio := found["audio:1"]
	if audio == nil || audio.GetSubscribable() {
		t.Fatalf("audio source = %+v, want not subscribable", audio)
	}
	if !strings.Contains(audio.GetDetail(), "not subscribable") {
		t.Errorf("unsubscribable source does not say why: %q", audio.GetDetail())
	}
}

// TestSensorAllowlistNarrowsTheGrant covers the case that makes this
// entitlement comparable to the camera entitlement's allowlist: an app that
// names its sources must neither reach nor even see the others.
func TestSensorAllowlistNarrowsTheGrant(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSourceProvider(func(context.Context) []data.Source {
		return []data.Source{
			{ID: "v4l2:/dev/video0", Kind: "camera", Healthy: true},
			{ID: "v4l2:/dev/video1", Kind: "camera", Healthy: true},
		}
	})
	service := NewSensorService("sh.wendy.test", manager)
	allowed := &fakeSensorProvider{sourceID: "v4l2:/dev/video0", samples: sensorScript("v4l2:/dev/video0", 1)}
	service.AddProvider(allowed)
	service.AddProvider(&fakeSensorProvider{sourceID: "v4l2:/dev/video1", samples: sensorScript("v4l2:/dev/video1", 1)})
	service.SetSourcePermission(func(id string) bool { return id == "v4l2:/dev/video0" })

	response, err := service.Sources(context.Background(), &appspbv1.SensorSourcesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range response.GetSources() {
		if source.GetId() == "v4l2:/dev/video1" {
			t.Error("Sources disclosed a camera outside the allowlist")
		}
	}

	stream := &fakeServerStream[appspbv1.SensorSample]{ctx: context.Background()}
	err = service.Subscribe(&appspbv1.SensorSubscribeRequest{SourceIds: []string{"v4l2:/dev/video1"}}, stream)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Subscribe to a source outside the allowlist returned %v, want PermissionDenied", err)
	}
	if len(stream.sent) != 0 {
		t.Fatalf("a refused subscription still delivered %d samples", len(stream.sent))
	}
	// The allowed source still works.
	if got := subscribeAll(t, service, allowed, &appspbv1.SensorSubscribeRequest{SourceIds: []string{"v4l2:/dev/video0"}}, 1); len(got) != 1 {
		t.Fatalf("allowlisted source delivered %d samples, want 1", len(got))
	}
}

// TestSensorSocketUnionsOwnerAllowlists documents the multi-service semantics:
// an app's services share one socket, so the socket permits the union of what
// they declared — and nothing beyond it. An owner with no allowlist is not a
// blanket grant any more; it cannot be created (Ensure refuses one) and, if one
// somehow appears, permits fails closed.
func TestSensorSocketUnionsOwnerAllowlists(t *testing.T) {
	socket := &appSensorSocket{owners: map[string][]string{"a": {"cam:1"}, "b": {"cam:2"}}}
	for _, id := range []string{"cam:1", "cam:2"} {
		if !socket.permits(id) {
			t.Errorf("union of owner allowlists does not permit %s", id)
		}
	}
	if socket.permits("cam:3") {
		t.Error("socket permitted a source no owner declared")
	}
	socket.owners["c"] = nil
	if socket.permits("cam:3") {
		t.Error("an owner with no allowlist widened the grant to every source")
	}
}

// TestEnsureRejectsAnEmptyAllowlist is the agent-side half of the mandatory
// allowlist. The CLI rejects a manifest without one, but the agent must not
// depend on that: a request that reached it another way still cannot obtain a
// grant covering every source the device offers.
func TestEnsureRejectsAnEmptyAllowlist(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := NewAppSensorSocketManager(ctx, zap.NewNop(), nil)
	for name, allowlist := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			if _, err := manager.Ensure("sh.wendy.test", "", allowlist); err == nil {
				t.Fatal("Ensure accepted a sensor-read owner with no allowlist")
			} else if !strings.Contains(err.Error(), "allowlist") {
				t.Fatalf("the refusal does not name the fix: %v", err)
			}
		})
	}
}

// TestSubscribeNamesAnUnimplementedSourceKind covers the audio and ROS 2 case:
// those sources exist, are captured into episodes, and have no producer that can
// multiplex to a model subscriber. An app that subscribes to one must be told
// that in as many words, rather than getting a message that reads like the
// source does not exist.
func TestSubscribeNamesAnUnimplementedSourceKind(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSourceProvider(func(context.Context) []data.Source {
		return []data.Source{{ID: "alsa:hw:0,0", Kind: "audio", Healthy: true}}
	})
	service := NewSensorService("sh.wendy.test", manager)
	service.AddProvider(&fakeSensorProvider{sourceID: "v4l2:/dev/video0"})

	stream := &fakeServerStream[appspbv1.SensorSample]{ctx: context.Background()}
	err = service.Subscribe(&appspbv1.SensorSubscribeRequest{SourceIds: []string{"alsa:hw:0,0"}}, stream)
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("Subscribe to an audio source returned %v, want Unimplemented", err)
	}
	for _, want := range []string{"audio", "not subscribable in this release"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}

	// A source the device does not have at all is a different problem and must
	// read as one.
	err = service.Subscribe(&appspbv1.SensorSubscribeRequest{SourceIds: []string{"alsa:hw:9,9"}}, stream)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("Subscribe to a nonexistent source returned %v, want NotFound", err)
	}
}
