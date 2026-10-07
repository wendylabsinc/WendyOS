package mcp

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
)

func fastCampaignBackend() *campaignWatchBackend {
	b := newCampaignWatchBackend(defaultWatchDetector)
	b.renewEvery, b.statusEvery, b.pollEvery = 20*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond
	return b
}

func testWatchSpec(name string) watchSpec {
	return watchSpec{Name: name, CameraID: "v4l2:/dev/video0", CameraName: "Brio 101", Classes: []string{"person", "dog"}, MinConfidence: 0.6, Label: "front door"}
}

// nextUpdate waits for the next update on h, failing after two seconds.
func nextUpdate(t *testing.T, h watchHandle) watchUpdate {
	t.Helper()
	select {
	case u, ok := <-h.Updates():
		if !ok {
			t.Fatal("updates closed")
		}
		return u
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a watch update")
		return watchUpdate{}
	}
}

// nextStatus skips events and gaps until a status arrives.
func nextStatus(t *testing.T, h watchHandle) watchStatusUpdate {
	t.Helper()
	for {
		if u := nextUpdate(t, h); u.Status != nil {
			return *u.Status
		}
	}
}

func TestCampaignWatchDeploysLeasedNotifyOnlyCampaign(t *testing.T) {
	client := newWatchDataClient()
	b := fastCampaignBackend()
	h, err := b.Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop(context.Background())
	c, ok := client.campaign("chat-0a1b2c3d-1")
	if !ok {
		t.Fatal("campaign was not deployed")
	}
	i := c.Inference
	if c.Lease != "60s" || !c.Leased() || len(c.Sources) != 1 || c.Sources[0].Camera != "v4l2:/dev/video0" ||
		i.Model != defaultWatchDetector.Model || i.Revision != defaultWatchDetector.Revision || i.Threshold != 0.6 || i.Rate != 2 ||
		len(i.Labels) != 2 || i.Labels[0] != "person" || i.Event != "chat-0a1b2c3d-1.detected" || i.ClearAfter != "5s" || i.Cooldown != "30s" ||
		c.Notify == nil || c.Notify.On != data.NotifyOnDetection || c.Notify.Webhook != "" {
		t.Fatalf("not the leased notify-only shape: %+v %+v", c, i)
	}
}

func TestCampaignWatchSendsOnlyTheWatchCampaignKeys(t *testing.T) {
	client := newWatchDataClient()
	h, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop(context.Background())
	client.mu.Lock()
	raw := client.deployedYAML["chat-0a1b2c3d-1"]
	client.mu.Unlock()
	var plan map[string]any
	if err := yaml.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	keys := func(m map[string]any) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if got, want := keys(plan), []string{"inference", "lease", "name", "notify", "sources", "version"}; !slices.Equal(got, want) {
		t.Fatalf("top-level keys %v, want %v\n%s", got, want, raw)
	}
	notify, _ := plan["notify"].(map[string]any)
	if got := keys(notify); !slices.Equal(got, []string{"on"}) {
		t.Fatalf("notify keys %v", got)
	}
}

// Start runs under startMu, so a hung deploy would block every other
// watch_start. It has the same deadline as the backend's other device calls.
func TestCampaignWatchDeployHasADeadline(t *testing.T) {
	client := newWatchDataClient()
	client.deployWait = 5 * time.Second
	b := fastCampaignBackend()
	b.callTimeout = 50 * time.Millisecond
	began := time.Now()
	h, err := b.Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
	if err == nil {
		h.Stop(context.Background())
		t.Fatal("a deploy past its deadline must fail")
	}
	if elapsed := time.Since(began); elapsed > time.Second || status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("got %v after %s", err, elapsed)
	}
	b.mu.Lock()
	watches, poller, starting := len(b.watches), b.poller, b.starting
	b.mu.Unlock()
	if watches != 0 || poller != nil || starting != 0 {
		t.Fatalf("a failed start left %d watches, poller %v, %d starts in flight", watches, poller, starting)
	}
	if names := client.deployedNames(); len(names) != 0 {
		t.Fatalf("deployed %v", names)
	}
}

func TestCampaignWatchOldAgentRefusesCleanly(t *testing.T) {
	for name, deployErr := range map[string]error{
		"unknown lease field": status.Error(codes.InvalidArgument, "parsing campaign YAML: yaml: unmarshal errors:\n  line 3: field lease not found in type data.Campaign"),
		"unimplemented":       status.Error(codes.Unimplemented, "unknown method"),
	} {
		t.Run(name, func(t *testing.T) {
			client := newWatchDataClient()
			client.deployErr = deployErr
			_, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
			if !errors.Is(err, errWatchAgentTooOld) {
				t.Fatalf("got %v, want errWatchAgentTooOld", err)
			}
		})
	}
	client := newWatchDataClient()
	client.deployErr = status.Error(codes.InvalidArgument, "no healthy camera matches")
	if _, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1")); err == nil || errors.Is(err, errWatchAgentTooOld) {
		t.Fatalf("an ordinary refusal was reported as an old agent: %v", err)
	}
	if _, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{}, testWatchSpec("chat-0a1b2c3d-1")); !errors.Is(err, errWatchAgentTooOld) {
		t.Fatalf("a connection without DataService: %v", err)
	}
}

func TestCampaignWatchMapsInferenceStatus(t *testing.T) {
	client := newWatchDataClient()
	client.setInference("chat-0a1b2c3d-1", data.InferenceStatus{State: "loading"})
	h, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop(context.Background())
	if s := nextStatus(t, h); s.State != watchPreparing {
		t.Fatalf("loading -> %+v", s)
	}
	client.setInference("chat-0a1b2c3d-1", data.InferenceStatus{State: "running"})
	if s := nextStatus(t, h); s.State != watchReady {
		t.Fatalf("running -> %+v", s)
	}
	client.setInference("chat-0a1b2c3d-1", data.InferenceStatus{State: "error", Error: "model download failed"})
	if s := nextStatus(t, h); s.State != watchError || s.Reason != "model download failed" {
		t.Fatalf("error -> %+v", s)
	}
	client.setInference("chat-0a1b2c3d-1", data.InferenceStatus{State: "waiting_for_cameras"})
	if s := nextStatus(t, h); s.State != watchError || s.Reason != "the camera is unavailable" {
		t.Fatalf("waiting_for_cameras -> %+v", s)
	}
}

// The agent retries a failed detector by loading it again every 5 s. The
// retry belongs to ERROR: a watch must not flap between ERROR and PREPARING.
func TestCampaignWatchStaysInErrorWhileTheAgentRetries(t *testing.T) {
	client := newWatchDataClient()
	name := "chat-0a1b2c3d-1"
	failed := data.InferenceStatus{State: "error", Error: "model download failed"}
	client.setInference(name, failed)
	h, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec(name))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop(context.Background())
	got := []watchStatusUpdate{nextStatus(t, h)}
	for range 4 {
		client.setInferenceSeen(t, name, data.InferenceStatus{State: "loading"})
		client.setInferenceSeen(t, name, failed)
		client.setInferenceSeen(t, name, data.InferenceStatus{State: "pending"})
	}
	client.setInferenceSeen(t, name, data.InferenceStatus{State: "error", Error: "out of memory"})
	client.setInferenceSeen(t, name, data.InferenceStatus{State: "loading"})
	client.setInference(name, data.InferenceStatus{State: "running"})
	for {
		s := nextStatus(t, h)
		got = append(got, s)
		if s.State == watchReady {
			break
		}
	}
	want := []watchStatusUpdate{{State: watchError, Reason: "model download failed"}, {State: watchError, Reason: "out of memory"}, {State: watchReady}}
	if !slices.Equal(got, want) {
		t.Fatalf("statuses %+v, want %+v", got, want)
	}
}

func TestWatchStateFromInferenceLoadingReasonFitsAWarmDevice(t *testing.T) {
	state, reason := watchStateFromInference(data.InferenceStatus{State: "loading"})
	if state != watchPreparing || reason != "loading the detector; a first watch on a device also installs it, which takes a few minutes" {
		t.Fatalf("loading -> %s %q", state, reason)
	}
}

func TestCampaignWatchRenewsAndEndsWhenTheLeaseIsGone(t *testing.T) {
	client := newWatchDataClient()
	h, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		client.mu.Lock()
		renews := client.renews["chat-0a1b2c3d-1"]
		client.mu.Unlock()
		if renews >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the lease was not renewed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	client.mu.Lock()
	delete(client.deployed, "chat-0a1b2c3d-1") // the agent restarted
	client.mu.Unlock()
	for {
		u, ok := <-h.Updates()
		if !ok {
			t.Fatal("updates closed without an ENDED status")
		}
		if u.Status != nil && u.Status.State == watchEnded {
			if u.Status.Reason != "the device restarted or the watch expired" {
				t.Fatalf("reason %q", u.Status.Reason)
			}
			break
		}
	}
	if _, ok := <-h.Updates(); ok {
		t.Fatal("updates stayed open after the watch ended")
	}
}

func TestCampaignWatchStopRemovesTheCampaign(t *testing.T) {
	client := newWatchDataClient()
	h, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := client.campaign("chat-0a1b2c3d-1"); ok {
		t.Fatal("campaign still deployed after Stop")
	}
	for range h.Updates() {
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("a second Stop must succeed (the campaign is already gone): %v", err)
	}
	client.removeErr = status.Error(codes.Unimplemented, "unknown method")
	h2, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h2.Stop(context.Background()); !errors.Is(err, errWatchAgentTooOld) {
		t.Fatalf("an agent without CampaignRemove: %v", err)
	}
}

// nextEvent skips statuses until an event or gap arrives.
func nextEvent(t *testing.T, h watchHandle) watchUpdate {
	t.Helper()
	for {
		if u := nextUpdate(t, h); u.Event != nil || u.Gap != "" {
			return u
		}
	}
}

func TestCampaignPollerStartsBeforeDeploy(t *testing.T) {
	client := newWatchDataClient()
	b := fastCampaignBackend()
	h, err := b.Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop(context.Background())
	calls := client.callLog()
	if len(calls) < 2 || calls[0] != "events:" || calls[1] != "deploy:chat-0a1b2c3d-1" {
		t.Fatalf("the journal tail must be read before the deploy: %v", calls)
	}
	client.publish("chat-0a1b2c3d-1", data.NotificationDetection{Label: "person", Score: 0.91})
	u := nextEvent(t, h)
	if u.Event == nil || u.Event.Kind != "entered" || len(u.Event.Classes) != 1 || u.Event.Classes[0] != (watchClass{Label: "person", Score: 0.91}) || u.Event.OccurredAt.IsZero() {
		t.Fatalf("event %+v", u.Event)
	}
}

func TestCampaignPollerRoutesByCampaignAndReportsGaps(t *testing.T) {
	client := newWatchDataClient()
	b := fastCampaignBackend()
	conn := &grpcclient.AgentConnection{DataService: client}
	first, err := b.Start(context.Background(), conn, testWatchSpec("chat-0a1b2c3d-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Stop(context.Background())
	second, err := b.Start(context.Background(), conn, testWatchSpec("chat-0a1b2c3d-2"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Stop(context.Background())
	if n := strings.Count(strings.Join(client.callLog(), " "), "events: "); n != 1 {
		t.Fatalf("a second watch must reuse the poller, not take a new tail; empty-cursor reads: %d", n)
	}
	client.publish("people-all-cameras", data.NotificationDetection{Label: "person", Score: 0.99}) // not a watch
	client.publish("chat-0a1b2c3d-2", data.NotificationDetection{Label: "dog", Score: 0.8})
	client.publish("chat-0a1b2c3d-1", data.NotificationDetection{Label: "person", Score: 0.7})
	if u := nextEvent(t, first); u.Event == nil || u.Event.Classes[0].Label != "person" {
		t.Fatalf("first watch got %+v", u)
	}
	if u := nextEvent(t, second); u.Event == nil || u.Event.Classes[0].Label != "dog" {
		t.Fatalf("second watch got %+v", u)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !slices.Contains(client.callLog(), "events:epoch:3") {
		if time.Now().After(deadline) {
			t.Fatalf("the poller did not advance its cursor past the entries it read: %v", client.callLog())
		}
		time.Sleep(5 * time.Millisecond)
	}
	client.mu.Lock()
	client.gapOnce = true
	client.mu.Unlock()
	for _, h := range []watchHandle{first, second} {
		if u := nextEvent(t, h); u.Gap != "some detections may have been missed" {
			t.Fatalf("gap not reported: %+v", u)
		}
	}
}

func TestCampaignPollerRefusesAnAgentWithoutTheJournal(t *testing.T) {
	client := newWatchDataClient()
	client.oldJournal = true
	_, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
	if !errors.Is(err, errWatchAgentTooOld) {
		t.Fatalf("got %v", err)
	}
	if slices.ContainsFunc(client.callLog(), func(c string) bool { return strings.HasPrefix(c, "deploy") }) {
		t.Fatal("a campaign was deployed to an agent without the notification journal")
	}
}

func TestCampaignPollerStopsWithTheLastWatch(t *testing.T) {
	client := newWatchDataClient()
	b := fastCampaignBackend()
	h, err := b.Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(client.callLog())
	time.Sleep(60 * time.Millisecond)
	if after := len(client.callLog()); after != before {
		t.Fatalf("the poller kept reading after the last watch stopped: %v", client.callLog()[before:])
	}
	client.deployErr = status.Error(codes.InvalidArgument, "no healthy camera matches")
	if _, err := b.Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-2")); err == nil {
		t.Fatal("expected the deploy to fail")
	}
	before = len(client.callLog())
	time.Sleep(60 * time.Millisecond)
	if after := len(client.callLog()); after != before {
		t.Fatal("a failed first deploy left the poller running")
	}
}

func TestCampaignPollerConcurrentStartsShareOnePoller(t *testing.T) {
	client := newWatchDataClient()
	b := fastCampaignBackend()
	conn := &grpcclient.AgentConnection{DataService: client}
	names := []string{"chat-0a1b2c3d-1", "chat-0a1b2c3d-2"}
	handles := make([]watchHandle, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handles[i], errs[i] = b.Start(context.Background(), conn, testWatchSpec(name))
		}()
	}
	wg.Wait()
	for i := range names {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		defer handles[i].Stop(context.Background())
	}
	if n := strings.Count(strings.Join(client.callLog(), " "), "events: "); n != 1 {
		t.Fatalf("concurrent starts must share one poller; empty-cursor reads: %d", n)
	}
	client.publish(names[0], data.NotificationDetection{Label: "person", Score: 0.9})
	if u := nextEvent(t, handles[0]); u.Event == nil {
		t.Fatalf("got %+v", u)
	}
	select {
	case u := <-handles[0].Updates():
		if u.Event != nil {
			t.Fatalf("the entry was routed twice: %+v", u)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCampaignPollerKeepsRunningForAStartInFlight(t *testing.T) {
	client := newWatchDataClient()
	b := fastCampaignBackend()
	conn := &grpcclient.AgentConnection{DataService: client}
	a, err := b.Start(context.Background(), conn, testWatchSpec("chat-0a1b2c3d-1"))
	if err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.deployWait = 150 * time.Millisecond
	client.mu.Unlock()
	type result struct {
		h   watchHandle
		err error
	}
	started := make(chan result, 1)
	go func() {
		h, err := b.Start(context.Background(), conn, testWatchSpec("chat-0a1b2c3d-2"))
		started <- result{h, err}
	}()
	time.Sleep(30 * time.Millisecond) // B is inside its deploy
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := <-started
	if r.err != nil {
		t.Fatal(r.err)
	}
	defer r.h.Stop(context.Background())
	client.publish("chat-0a1b2c3d-2", data.NotificationDetection{Label: "dog", Score: 0.8})
	if u := nextEvent(t, r.h); u.Event == nil || u.Event.Classes[0].Label != "dog" {
		t.Fatalf("got %+v", u)
	}
}
