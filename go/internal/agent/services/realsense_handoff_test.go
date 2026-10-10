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

	s.vacateForOwnershipChange([]string{"/dev/video4", "/dev/video5"})

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
	bridge.vacate = func(nodes []string) {
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
