package services

import (
	"context"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/framesource"
	"github.com/wendylabsinc/wendy/go/internal/agent/framesource/framesourcetest"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestCameraOwnerTable_ClaimHolderRelease(t *testing.T) {
	table := newCameraOwnerTable()

	if _, ok := table.holder("/dev/video0"); ok {
		t.Fatal("empty table reported an owner")
	}

	table.claim("realsense-123", framesource.KindRealSense, []string{"/dev/video0", "/dev/video2"})
	owner, ok := table.holder("/dev/video2")
	if !ok || owner.Source != "realsense-123" {
		t.Fatalf("holder = %+v, %v; want realsense-123", owner, ok)
	}

	table.release("realsense-123")
	if _, ok := table.holder("/dev/video0"); ok {
		t.Fatal("released node still reports an owner")
	}
}

// A later claim of the same node describes whoever actually holds the device
// now, and the earlier claimant's release must not evict it.
func TestCameraOwnerTable_LaterClaimWinsAndSurvivesEarlierRelease(t *testing.T) {
	table := newCameraOwnerTable()
	table.claim("first", framesource.KindRealSense, []string{"/dev/video0"})
	table.claim("second", framesource.KindRealSense, []string{"/dev/video0"})

	if owner, _ := table.holder("/dev/video0"); owner.Source != "second" {
		t.Fatalf("holder = %q, want second", owner.Source)
	}

	table.release("first")
	if owner, ok := table.holder("/dev/video0"); !ok || owner.Source != "second" {
		t.Fatalf("after first's release holder = %+v, %v; want second intact", owner, ok)
	}
}

// A RealSense capture claims the camera's nodes for exactly the capture's
// lifetime: present while the producer runs, gone once the helper has released
// the device (Shutdown waits for that).
func TestCalibratedCapture_ClaimsAndReleasesRealSenseNodes(t *testing.T) {
	prevNodes := framesource.RealSenseNodePaths
	framesource.RealSenseNodePaths = func() []string { return []string{"/dev/video9"} }
	t.Cleanup(func() { framesource.RealSenseNodePaths = prevNodes })
	t.Cleanup(func() { cameraOwners.release("fake:1") })

	fake := framesourcetest.NewFake("fake:1", 64, 48)
	fake.Desc().Kind = framesource.KindRealSense
	svc := newCalibratedService(fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, newStreamStub(ctx))

	waitForCond(t, "the capture to claim the camera's nodes", func() bool {
		owner, ok := cameraOwners.holder("/dev/video9")
		return ok && owner.Source == "fake:1" && owner.Kind == framesource.KindRealSense
	})

	svc.Shutdown()
	<-errCh
	if _, ok := cameraOwners.holder("/dev/video9"); ok {
		t.Fatal("capture ended but the claim is still in the table")
	}
}

// A non-RealSense capture claims nothing: the table is about devices the
// helper takes exclusively, not every source.
func TestCalibratedCapture_FakeKindClaimsNothing(t *testing.T) {
	prevNodes := framesource.RealSenseNodePaths
	framesource.RealSenseNodePaths = func() []string { return []string{"/dev/video9"} }
	t.Cleanup(func() { framesource.RealSenseNodePaths = prevNodes })

	fake := framesourcetest.NewFake("fake:1", 64, 48)
	svc := newCalibratedService(fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := run(svc, &agentpbv2.StreamCalibratedFramesRequest{Source: "fake:1"}, newStreamStub(ctx))
	waitForSubscribers(t, svc, "fake:1", 1)

	if _, ok := cameraOwners.holder("/dev/video9"); ok {
		t.Fatal("a fake-kind capture claimed a node")
	}
	svc.Shutdown()
	<-errCh
}

// The in-use refusal names the agent's own capture when the table knows the
// holder, instead of sending an operator hunting for "another application".
func TestErrCameraInUse_NamesCalibratedOwner(t *testing.T) {
	cameraOwners.claim("realsense-123", framesource.KindRealSense, []string{"/dev/video7"})
	t.Cleanup(func() { cameraOwners.release("realsense-123") })

	err := errCameraInUse("/dev/video7")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "realsense-123") {
		t.Fatalf("refusal does not name the owning source: %v", err)
	}
	if !strings.Contains(err.Error(), "StreamCalibratedFrames") {
		t.Fatalf("refusal does not say which stream holds the camera: %v", err)
	}
}
