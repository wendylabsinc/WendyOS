package mcp

import (
	"context"
	"errors"
	"slices"
	"sort"
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
