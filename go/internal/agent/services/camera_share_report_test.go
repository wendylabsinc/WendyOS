package services

import (
	"context"
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestCameraShareReport_NamesModeAppsAndStates(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.writeState(`{"version":1,"containers":{` +
		`"cv-b":{"app":"sh.wendy.cv-b","shares":[` + csStateRecord(csBrio(), "/dev/video0", testSharedMode, 253) + `]},` +
		`"cv-a":{"app":"sh.wendy.cv-a","shares":[` + csStateRecord(csBrio(), "/dev/video0", testSharedMode, 254) + `]},` +
		`"cv-c":{"app":"sh.wendy.cv-c","shares":[` + csStateRecord(csBrio(), "/dev/video0", testSharedMode, 252) + `]}}}`)
	h.s.loadCameraShares()
	h.s.shares.mu.Lock()
	h.s.shares.containers["cv-a"].shares[0].pump = &sharePump{state: shareState{Phase: shareReading}}
	h.s.shares.containers["cv-b"].shares[0].pump = &sharePump{state: shareState{Phase: shareWaiting, Detail: "held by cv-x at 1920x1080"}}
	h.s.shares.containers["cv-c"].shares[0].pump = &sharePump{state: shareState{Phase: shareIdle}}
	h.s.shares.containers["cv-c"].shares[0].tookRealCamera = "python3"
	h.s.shares.mu.Unlock()

	got := h.s.CameraShareReport(context.Background())["/dev/video0"]
	dev := &agentpb.VideoDevice{Path: "/dev/video0", Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_USB}
	want := map[string]string{
		"share_mode":  h.s.shareDecisionFor(context.Background(), dev).Mode.String(),
		"shared_with": "cv-a, cv-b, cv-c",
		"share_state": "cv-a (reading), cv-b (waiting: held by cv-x at 1920x1080), cv-c (not sharing: took the real camera after a replug)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("report = %#v, want %#v", got, want)
	}
}

func TestCameraShareReport_UnsharedCameraSaysWhy(t *testing.T) {
	h := newCSHarness(t, csH264Cam())
	got := h.s.CameraShareReport(context.Background())["/dev/video2"]
	want := map[string]string{"share_unavailable": "the camera produces H.264 itself; sharing it comes in a later release"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("report = %#v, want %#v", got, want)
	}
}

// Two resolves read the same old camera; the first follows it for every
// record, so the second finds its record already moved and is still resolved.
func TestResolveShareCamera_ConcurrentFollowStillResolves(t *testing.T) {
	h := newCSHarness(t, csBrio())
	moved := camera.Identity{Device: csPort("1-2.9"), Index: "0", Name: "Brio 101"}
	h.writeState(`{"version":1,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
		csStateRecord(csBrio(), "/dev/video0", testSharedMode, 254) + `]}}}`)
	h.s.loadCameraShares()
	r := h.records("cv-a")[0]
	present := map[string]camera.Identity{"video0": moved}

	prev := afterShareResolveRead
	t.Cleanup(func() { afterShareResolveRead = prev })
	afterShareResolveRead = func() {
		afterShareResolveRead = prev
		if base, ok, _ := h.s.resolveShareCamera(r, present); !ok || base != "video0" {
			t.Errorf("inner resolve = %q, %v", base, ok)
		}
	}
	base, ok, _ := h.s.resolveShareCamera(r, present)
	if !ok || base != "video0" {
		t.Errorf("outer resolve, whose record the inner one moved = %q, %v; want video0, true", base, ok)
	}
	h.s.shares.mu.Lock()
	got := r.camera
	h.s.shares.mu.Unlock()
	if got != moved {
		t.Errorf("record follows %+v, want %+v", got, moved)
	}
}
