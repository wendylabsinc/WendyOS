package services

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
)

// vacatedHub builds a deviceHub whose "producer" releases the device (closes
// done) as soon as it is cancelled, the way runProducer's teardown does.
func vacatedHub(t *testing.T) *deviceHub {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &deviceHub{
		subs:     map[int]*hubSubscriber{},
		subDrops: map[int]uint64{},
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	go func() {
		<-ctx.Done()
		close(h.done)
	}()
	return h
}

// An ownership handoff ends exactly the producers on the claimed nodes, marks
// them restarted with the handoff's own message, and waits for the device to
// be released before returning -- librealsense opens it immediately after.
func TestVacateForOwnershipChange_EndsOnlyTheClaimedNodes(t *testing.T) {
	claimed := vacatedHub(t)
	unrelated := vacatedHub(t)
	s := &VideoService{
		logger: zap.NewNop(),
		hubs: map[string]*deviceHub{
			"/dev/video4": claimed,
			"/dev/video7": unrelated,
		},
	}

	s.vacateForOwnershipChange([]string{"/dev/video4", "/dev/video5"},
		"video stream ended: a calibrated frame capture took ownership of this camera; reconnect to join the stream served from it")

	select {
	case <-claimed.done:
	default:
		t.Fatal("vacate returned before the claimed producer released its device")
	}
	if !claimed.wasRestarted() {
		t.Fatal("the vacated hub is not marked restarted; its subscribers would read the end as a fault")
	}
	if msg := claimed.restartMessage(); !strings.Contains(msg, "calibrated frame capture took ownership") {
		t.Fatalf("restart message = %q; want it to name the handoff, not episode capture", msg)
	}
	if _, ok := s.hubs["/dev/video4"]; ok {
		t.Fatal("the vacated hub is still installed; a reconnect would join a dead producer")
	}

	if unrelated.ctx.Err() != nil || unrelated.wasRestarted() {
		t.Fatal("a node the capture never claimed was vacated")
	}
	if _, ok := s.hubs["/dev/video7"]; !ok {
		t.Fatal("the unrelated hub was removed")
	}
}

// A hub that was never restarted keeps the episode-capture default message, so
// the existing takeover path reads exactly as before.
func TestRestartMessage_DefaultsToEpisodeCapture(t *testing.T) {
	h := &deviceHub{}
	h.mu.Lock()
	h.restarted = true
	h.mu.Unlock()
	if msg := h.restartMessage(); !strings.Contains(msg, "episode capture restarted") {
		t.Fatalf("restart message = %q; want the episode-capture default", msg)
	}
}

// The bridge installs its redirect before asking the video service to vacate,
// so a subscriber bounced by the handoff can only land on the bridge node.
func TestRealSenseBridge_VacateSeesRedirect(t *testing.T) {
	t.Cleanup(func() { cameraOwners.release("fake:1") })
	loop := newFakeLoopback()
	loop.auxNext = 250
	writer := &fakeBridgeWriter{}
	bridge := newRealSenseBridge(zap.NewNop(), loop)
	bridge.writerFor = func(string) ros2camera.CameraWriter { return writer }
	bridge.colourNodeFor = func([]string) string { return "/dev/video4" }

	redirectAtVacate := ""
	bridge.vacate = func(nodes []string, _ string) {
		redirectAtVacate, _ = cameraOwners.redirect("/dev/video4")
	}

	cameraOwners.claim("fake:1", "realsense", []string{"/dev/video4"})
	hub := &frameHub{subs: map[int]*frameSub{}, source: "fake:1"}
	bc := bridge.start(context.Background(), hub, "fake:1", []string{"/dev/video4"})
	if bc == nil {
		t.Fatal("bridge did not start")
	}
	defer bc.teardown()

	if redirectAtVacate != "/dev/video250" {
		t.Fatalf("at vacate time the redirect was %q; want the bridge node already installed", redirectAtVacate)
	}
}

// Teardown bounces the bridge node's own subscribers with the recoverable
// restart signal -- redirect already cleared, so their reconnect resolves the
// real device -- before the node is removed from under them.
func TestRealSenseBridge_TeardownBouncesBridgeSubscribers(t *testing.T) {
	t.Cleanup(func() { cameraOwners.release("fake:1") })
	loop := newFakeLoopback()
	loop.auxNext = 250
	writer := &fakeBridgeWriter{}
	bridge := newRealSenseBridge(zap.NewNop(), loop)
	bridge.writerFor = func(string) ros2camera.CameraWriter { return writer }
	bridge.colourNodeFor = func([]string) string { return "/dev/video4" }

	type vacateCall struct {
		nodes       []string
		msg         string
		redirectSet bool
	}
	var calls []vacateCall
	bridge.vacate = func(nodes []string, msg string) {
		_, ok := cameraOwners.redirect("/dev/video4")
		calls = append(calls, vacateCall{nodes: nodes, msg: msg, redirectSet: ok})
	}

	hub := &frameHub{subs: map[int]*frameSub{}, source: "fake:1"}
	bc := bridge.start(context.Background(), hub, "fake:1", []string{"/dev/video4"})
	if bc == nil {
		t.Fatal("bridge did not start")
	}
	bc.teardown()

	if len(calls) != 2 {
		t.Fatalf("vacate called %d times, want start + teardown", len(calls))
	}
	td := calls[1]
	if len(td.nodes) != 1 || td.nodes[0] != "/dev/video250" {
		t.Fatalf("teardown vacated %v, want the bridge node", td.nodes)
	}
	if !strings.Contains(td.msg, "capture serving this camera ended") {
		t.Fatalf("teardown bounce message = %q; want it to say the capture ended", td.msg)
	}
	if td.redirectSet {
		t.Fatal("teardown bounced subscribers while the redirect still stood; a reconnect would chase a vanishing node")
	}
	if len(loop.auxRemoved) == 0 || loop.auxRemoved[0] != 250 {
		t.Fatalf("auxRemoved = %v, want the bridge node removed after the bounce", loop.auxRemoved)
	}
}
