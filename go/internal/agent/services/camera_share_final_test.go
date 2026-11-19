package services

// Tests for the final whole-branch review's fixes to the share manager: status
// that matches what the app gets, records that follow only their own camera,
// a failed start cover, node access and removal, and the state file's version.

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/devwatch"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// csOtherAtVideo0 is a C920 that took /dev/video0 after the Brio left it.
func csOtherAtVideo0() csCamera {
	c := csC920()
	c.base = "video0"
	return c
}

func (h *csHarness) feedOverflow() {
	h.watcher.mu.Lock()
	onEvent := h.watcher.onEvent
	h.watcher.mu.Unlock()
	onEvent(devwatch.Event{Overflow: true})
}

func (h *csHarness) reportOf(path string) map[string]string {
	return h.s.CameraShareReport(context.Background())[path]
}

// I2: a camera that takes the path of a shared camera that went is not held to
// the shared camera's mode, and the listing does not name the shared camera's
// apps on its row; when the shared camera comes back, both return.
func TestSharedMode_ACameraSwappedAtThePathDoesNotInheritTheRules(t *testing.T) {
	wide := &agentpb.StreamVideoRequest{Width: 1920, Height: 1080}
	refused := func(h *csHarness) bool {
		_, err := h.s.applySharedMode("/dev/video0", wide)
		return status.Code(err) == codes.InvalidArgument
	}

	t.Run("seen by the /dev watch", func(t *testing.T) {
		h := newCSHarness(t, csBrio())
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
		h.watch()
		if !refused(h) {
			t.Fatal("before the swap, 1920x1080 on the shared camera was admitted")
		}

		h.unplug("video0")
		h.plug(csOtherAtVideo0())
		h.waitLogged("camera share: /dev/video0 appeared; no running app shares it", 1)
		if got, err := h.s.applySharedMode("/dev/video0", wide); err != nil || got != wide {
			t.Fatalf("the C920 now at /dev/video0 is held to the Brio's mode: %v, %v", got, err)
		}
		if _, _, ok := h.s.sharedModeOf("/dev/video0"); ok {
			t.Fatal("sharedModeOf still reports /dev/video0 as shared")
		}
		if row := h.reportOf("/dev/video0"); row["shared_with"] != "" || row["share_state"] != "" {
			t.Fatalf("the C920's row names the Brio's sharers: %#v", row)
		}

		h.unplug("video0")
		h.plug(csBrio())
		h.waitLogged("camera share: /dev/video0 appeared; no running app shares it", 2)
		if !refused(h) {
			t.Fatal("the Brio came back and 1920x1080 on it was admitted")
		}
		if row := h.reportOf("/dev/video0"); row["shared_with"] != "cv-a" {
			t.Fatalf("the Brio's row after it came back = %#v, want cv-a sharing", row)
		}
	})

	t.Run("seen by the listing and the sync, with no watch", func(t *testing.T) {
		h := newCSHarness(t, csBrio())
		h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
		listed := []CameraShareContainer{{ID: "cv-a"}}

		h.takeOut("video0")
		h.mu.Lock()
		h.cams["video0"] = csOtherAtVideo0()
		h.mu.Unlock()
		if row := h.reportOf("/dev/video0"); row["shared_with"] != "" {
			t.Fatalf("the C920's row names the Brio's sharers: %#v", row)
		}
		h.s.SyncCameraShares(context.Background(), listed)
		if refused(h) {
			t.Fatal("after a sync, the C920 at /dev/video0 is still held to the Brio's mode")
		}

		h.takeOut("video0")
		h.mu.Lock()
		h.cams["video0"] = csBrio()
		h.mu.Unlock()
		h.s.SyncCameraShares(context.Background(), listed)
		if !refused(h) {
			t.Fatal("the Brio came back, and after a sync 1920x1080 on it was admitted")
		}
		if row := h.reportOf("/dev/video0"); row["shared_with"] != "cv-a" {
			t.Fatalf("the Brio's row after it came back = %#v, want cv-a sharing", row)
		}
	})
}

// I1: an adopted node keeps the mode an earlier agent stored. When a share made
// now would get another, the agent says so and the listing tells the user to
// redeploy; when the modes agree it says nothing.
func TestSyncCameraShares_AnAdoptedNodeAtAnOldModeAsksForARedeploy(t *testing.T) {
	dev := &agentpb.VideoDevice{Path: "/dev/video0", Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_USB}
	// today is the decision a share made now gets, taken before the sync starts
	// the check (the test's decision fakes write package state).
	adopt := func(t *testing.T, stored func(today sharedMode) sharedMode) (h *csHarness, today sharedMode) {
		h = newCSHarness(t, csBrio())
		today = h.s.shareDecisionFor(context.Background(), dev).Mode
		h.writeState(`{"version":1,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
			csStateRecord(csBrio(), "/dev/video0", stored(today), 254) + `]}}}`)
		h.loop.addLiveAux(254, "Brio 101")
		h.s.loadCameraShares()
		h.s.SyncCameraShares(context.Background(), []CameraShareContainer{{ID: "cv-a", Running: true, PID: 4242}})
		return h, today
	}

	t.Run("another mode today", func(t *testing.T) {
		old := sharedMode{Width: 640, Height: 480, Fourcc: "YUYV", BytesPerLine: 1280, FPS: 15}
		h, today := adopt(t, func(sharedMode) sharedMode { return old })
		h.waitLogged("but a share made now would get "+today.String(), 1)
		h.s.Shutdown() // the check has finished; the listing below decides again
		want := "cv-a (idle; redeploy the app: its node keeps 640x480 YUYV, 15 fps, and the camera's shared mode is now " + today.String() + ")"
		if got := h.reportOf("/dev/video0")["share_state"]; got != want {
			t.Fatalf("share_state = %q\nwant          %q", got, want)
		}
	})

	t.Run("the same mode", func(t *testing.T) {
		h, _ := adopt(t, func(today sharedMode) sharedMode { return today })
		h.s.Shutdown() // waits for the check
		if n := h.logged("a share made now would get"); n != 0 {
			t.Fatalf("a mode-change line for a node at today's mode: %v", h.logs.All())
		}
		if v := h.view("cv-a"); len(v) != 1 {
			t.Fatalf("records = %+v", v)
		}
		h.s.shares.mu.Lock()
		note := h.s.shares.containers["cv-a"].shares[0].modeNote
		h.s.shares.mu.Unlock()
		if note != "" {
			t.Fatalf("modeNote = %q, want none", note)
		}
	})
}

// M1: a start whose cover failed leaves the app on the real camera for its run;
// the listing says so, and a sync covers it again once the start is surely done.
func TestCoverCameraShares_AFailedStartCoverIsShownAndRetried(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.setCoverErr("/dev/video0", errors.New("move_mount(-> /dev/video0): invalid argument"))
	if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 4242); len(errs) != 1 {
		t.Fatalf("errors = %v, want one", errs)
	}
	want := "cv-a (not sharing: its start's cover failed: move_mount(-> /dev/video0): invalid argument)"
	if got := h.reportOf("/dev/video0")["share_state"]; got != want {
		t.Fatalf("share_state = %q\nwant          %q", got, want)
	}

	h.setCoverErr("/dev/video0", nil)
	h.setCovered(false)
	listed := []CameraShareContainer{{ID: "cv-a", Running: true, PID: 4242}}
	h.s.SyncCameraShares(context.Background(), listed)
	if got := h.coverList(); len(got) != 0 {
		t.Fatalf("a sync right after the start covered %+v; the start's own cover may still land", got)
	}
	h.ageNamespaces(cameraShareCoverRetryAge)
	h.s.SyncCameraShares(context.Background(), listed)
	if got, want := h.coverList(), []csCover{{4242, "/dev/video255", "/dev/video0", cameraShareRecoverTimeout}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("covers = %+v, want %+v", got, want)
	}
	h.waitLogged("camera share: covered /dev/video0 for cv-a again; its start's cover had failed", 1)
	if got := h.reportOf("/dev/video0")["share_state"]; got != "cv-a (idle)" {
		t.Fatalf("share_state after the retry = %q, want cv-a (idle)", got)
	}
	h.s.SyncCameraShares(context.Background(), listed)
	if n := len(h.coverList()); n != 1 {
		t.Fatalf("%d covers after a third sync, want the retry's alone", n)
	}
}

// M1: the next start's cover clears the note, and so does the run ending.
func TestCoverCameraShares_AFailedStartCoverNoteEndsWithTheRun(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.setCoverErr("/dev/video0", errors.New("boom"))
	h.s.CoverCameraShares(context.Background(), "cv-a", 11)
	if h.reportOf("/dev/video0")["share_state"] == "cv-a (idle)" {
		t.Fatal("no note after a failed start cover")
	}
	h.s.ReleaseCameraShares("cv-a")
	if got := h.reportOf("/dev/video0")["share_state"]; got != "cv-a (idle)" {
		t.Fatalf("after the task exited: %q, want cv-a (idle)", got)
	}
	h.s.CoverCameraShares(context.Background(), "cv-a", 12)
	h.setCoverErr("/dev/video0", nil)
	if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 13); len(errs) != 0 {
		t.Fatalf("errors = %v", errs)
	}
	if got := h.reportOf("/dev/video0")["share_state"]; got != "cv-a (idle)" {
		t.Fatalf("after a start that covered: %q, want cv-a (idle)", got)
	}
}

// M2: a new share node gets udev's owner group and mode at once; a failure is
// logged and the share is made all the same.
func TestCreateCameraShares_ANewNodeGetsUdevsAccessAtOnce(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	if got := h.entryLog(); !reflect.DeepEqual(got, []string{"access /dev/video255"}) {
		t.Fatalf("entry operations = %v, want access /dev/video255", got)
	}

	h.setEntryErrs(errors.New("looking up the video group: group: unknown group video"), nil, nil)
	h.s.CreateCameraShares(context.Background(), "cv-b", "sh.wendy.cv-b")
	if n := len(h.records("cv-b")); n != 1 {
		t.Fatalf("cv-b has %d shares, want 1 although the access change failed", n)
	}
	if n := h.logged("camera share: giving /dev/video254 udev's owner group and mode (root:video 0660) failed: looking up the video group"); n != 1 {
		t.Fatalf("access failure logged %d times; logs: %v", n, h.logs.All())
	}
}

// M4: a node's /dev entry goes just before CTL_REMOVE; a removal the driver
// refuses gets the entry back and stays queued, and the next attempt removes
// the node.
func TestRemoveShareNode_UnlinksItsEntryFirstAndRestoresItWhenRefused(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.loop.mu.Lock()
	h.loop.refuseRemove = true
	h.loop.mu.Unlock()
	h.s.RemoveCameraShares("cv-a")
	h.drainRemovals()
	want := []string{"access /dev/video255", "unlink 255 81:255", "remove 255", "mknod 255 81:255", "access /dev/video255"}
	if got := h.entryLog(); !reflect.DeepEqual(got, want) {
		t.Fatalf("entry operations = %v\nwant                   %v", got, want)
	}
	if !reflect.DeepEqual(h.pendingRemovals(), []int{255}) || !h.s.IsShareNode("/dev/video255") {
		t.Fatalf("pending %v; want 255 still queued and a share node", h.pendingRemovals())
	}

	h.loop.mu.Lock()
	h.loop.refuseRemove = false
	h.loop.mu.Unlock()
	h.s.SyncCameraShares(context.Background(), nil)
	h.drainRemovals()
	want = append(want, "unlink 255 81:255", "remove 255")
	if got := h.entryLog(); !reflect.DeepEqual(got, want) {
		t.Fatalf("entry operations = %v\nwant                   %v", got, want)
	}
	if live, pending := h.loop.liveAux(), h.pendingRemovals(); len(live) != 0 || len(pending) != 0 {
		t.Fatalf("live %v, pending %v; want the node gone", live, pending)
	}
}

// M4: an entry the agent made again is never left behind: when its node goes
// some other way, the next attempt removes the entry.
func TestRemoveShareNode_ARestoredEntryWhoseNodeWentIsRemoved(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.loop.mu.Lock()
	h.loop.refuseRemove = true
	h.loop.mu.Unlock()
	h.s.RemoveCameraShares("cv-a")
	h.drainRemovals()
	h.loop.vanishKeepingEntry(255) // the kernel never removes an entry the agent made
	h.loop.mu.Lock()
	h.loop.refuseRemove = false
	h.loop.mu.Unlock()
	h.s.SyncCameraShares(context.Background(), nil)
	h.drainRemovals()
	want := []string{"access /dev/video255", "unlink 255 81:255", "remove 255", "mknod 255 81:255", "access /dev/video255", "unlink 255 81:255"}
	if got := h.entryLog(); !reflect.DeepEqual(got, want) {
		t.Fatalf("entry operations = %v\nwant                   %v", got, want)
	}
	if pending := h.pendingRemovals(); len(pending) != 0 {
		t.Fatalf("pending %v, want none", pending)
	}
}

// M4 re-review: a removal that was refused and whose entry could not be made
// again leaves a node with no /dev entry. The next attempt still removes it:
// gone is read from sysfs, not from /dev.
func TestRemoveShareNode_ANodeLeftWithoutItsEntryIsStillRemoved(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.loop.mu.Lock()
	h.loop.refuseRemove = true
	h.loop.mu.Unlock()
	h.setEntryErrs(nil, nil, errors.New("mknod: no space left on device"))
	h.s.RemoveCameraShares("cv-a")
	h.drainRemovals()
	if _, ok := h.loop.NodePath(255); ok || !reflect.DeepEqual(h.loop.liveAux(), []int{255}) {
		t.Fatalf("setup: want node 255 in the kernel with no /dev entry; live %v", h.loop.liveAux())
	}

	h.loop.mu.Lock()
	h.loop.refuseRemove = false
	h.loop.mu.Unlock()
	h.setEntryErrs(nil, nil, nil)
	h.s.SyncCameraShares(context.Background(), nil)
	h.drainRemovals()
	if got := h.loop.removedAux(); !reflect.DeepEqual(got, []int{255, 255}) {
		t.Fatalf("CTL_REMOVE calls = %v, want 255 twice (refused, then taken)", got)
	}
	if live, pending := h.loop.liveAux(), h.pendingRemovals(); len(live) != 0 || len(pending) != 0 {
		t.Fatalf("live %v, pending %v; want the node gone", live, pending)
	}
}

// M4 re-review: an agent that died between unlinking a node's entry and
// CTL_REMOVE left the removal in the state file and the node in the kernel
// with no entry; the next agent removes it.
func TestRemoveShareNode_AnAgentThatDiedMidRemovalLeavesNothingBehind(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.writeState(`{"version":1,"containers":{},"removing":[{"node":254,"name":"Brio 101"}]}`)
	h.loop.addLiveAux(254, "Brio 101")
	h.loop.setEntry(254, false)
	h.s.loadCameraShares()

	h.s.SyncCameraShares(context.Background(), nil)
	h.drainRemovals()
	if got := h.loop.removedAux(); !reflect.DeepEqual(got, []int{254}) {
		t.Fatalf("CTL_REMOVE calls = %v, want 254", got)
	}
	if live, pending := h.loop.liveAux(), h.pendingRemovals(); len(live) != 0 || len(pending) != 0 {
		t.Fatalf("live %v, pending %v; want the node gone", live, pending)
	}
	if removing := h.stateFile()["removing"]; removing != nil {
		t.Fatalf("the state file still queues %v", removing)
	}
}

// M4 re-review: share allocation never hands out a number the kernel still
// has, though its /dev entry is gone (the allocator stats /dev, and CTL_ADD
// there would answer EEXIST).
func TestCreateCameraShares_NeverTakesANumberWhoseNodeLostItsEntry(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.loop.addLiveAux(255, "Brio 101") // no record, no queued removal
	h.loop.setEntry(255, false)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	recs := h.records("cv-a")
	if len(recs) != 1 || recs[0].node != 254 {
		t.Fatalf("records = %+v, want one share at node 254", recs)
	}

	t.Run("nor makes a node lost in a reboot again at such a number", func(t *testing.T) {
		h := newCSHarness(t, csBrio())
		h.writeState(`{"version":1,"containers":{"cv-a":{"app":"sh.wendy.cv-a","shares":[` +
			csStateRecord(csBrio(), "/dev/video0", testSharedMode, 254) + `]}}}`)
		h.s.loadCameraShares()
		h.loop.addLiveAux(254, "Brio 101") // in the kernel, entry gone
		h.loop.setEntry(254, false)
		h.s.CoverCameraShares(context.Background(), "cv-a", 4242)
		h.drainCovers()
		if v := h.view("cv-a"); len(v) != 1 || v[0].node == 254 {
			t.Fatalf("records = %+v, want the node made again at another number", v)
		}
	})
}

// M4 re-review: an attempt that stops before the removal (a holder) keeps
// track of the entry it made again, so the next attempt still finds it stale
// once the node has gone.
func TestRemoveShareNode_ARestoredEntryIsTrackedAcrossAnAttemptThatStopsEarly(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.loop.mu.Lock()
	h.loop.refuseRemove = true
	h.loop.mu.Unlock()
	h.s.RemoveCameraShares("cv-a")
	h.drainRemovals()

	h.setHeld("/dev/video255", deviceHolder{PID: 99, Comm: "wireplumber"})
	h.s.SyncCameraShares(context.Background(), nil) // the holder stops it
	h.drainRemovals()
	h.setHeld("/dev/video255")
	h.loop.vanishKeepingEntry(255)
	h.loop.mu.Lock()
	h.loop.refuseRemove = false
	h.loop.mu.Unlock()
	h.s.SyncCameraShares(context.Background(), nil)
	h.drainRemovals()
	log := h.entryLog()
	if last := log[len(log)-1]; last != "unlink 255 81:255" || h.loop.hasEntry(255) {
		t.Fatalf("entry operations = %v; want the stale entry unlinked last", log)
	}
	if pending := h.pendingRemovals(); len(pending) != 0 {
		t.Fatalf("pending %v, want none", pending)
	}
}

// M4: an entry that cannot be made again is logged; the removal stays queued.
func TestRemoveShareNode_AnEntryThatCannotBeMadeAgainIsLogged(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.loop.mu.Lock()
	h.loop.refuseRemove = true
	h.loop.mu.Unlock()
	h.setEntryErrs(nil, nil, errors.New("mknod: read-only file system"))
	h.s.RemoveCameraShares("cv-a")
	h.drainRemovals()
	if n := h.logged("camera share: making /dev/video255 again after its removal did not take failed: mknod: read-only file system"); n != 1 {
		t.Fatalf("restore failure logged %d times; logs: %v", n, h.logs.All())
	}
	if !reflect.DeepEqual(h.pendingRemovals(), []int{255}) {
		t.Fatalf("pending %v, want 255", h.pendingRemovals())
	}
}

// M5: a state file a newer agent wrote is left as it is, read by no one here,
// and never written over; this agent's shares live in memory.
func TestCameraShareState_ANewerVersionIsLeftInPlace(t *testing.T) {
	h := newCSHarness(t, csBrio())
	newer := `{"version":2,"containers":{"cv-x":{"app":"sh.wendy.cv-x","shares":[]}},"somethingNew":true}`
	h.writeState(newer)
	h.s.loadCameraShares()
	if ids := h.containerIDs(); len(ids) != 0 {
		t.Fatalf("records from a newer file: %v", ids)
	}
	if _, err := os.Stat(cameraSharesPath + ".corrupt"); !os.IsNotExist(err) {
		t.Fatalf("the newer file was set aside as corrupt: %v", err)
	}
	if n := h.logged("is version 2, written by a newer agent; this agent reads version 1"); n != 1 {
		t.Fatalf("version line logged %d times; logs: %v", n, h.logs.All())
	}

	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.s.RemoveCameraShares("cv-a")
	h.drainRemovals()
	if got, err := os.ReadFile(cameraSharesPath); err != nil || string(got) != newer {
		t.Fatalf("the newer file was written over: %q, %v", got, err)
	}
	if n := len(h.loop.shareCalls()); n != 1 {
		t.Fatalf("%d nodes made, want the one share in memory", n)
	}

	t.Run("an older or missing version is still set aside", func(t *testing.T) {
		h := newCSHarness(t, csBrio())
		h.writeState(`{"containers":{}}`)
		h.s.loadCameraShares()
		if _, err := os.Stat(cameraSharesPath + ".corrupt"); err != nil {
			t.Fatalf("no .corrupt file: %v", err)
		}
	})
}

// M9: a pump waiting out a backoff because its camera was unplugged joins as
// soon as the camera's node appears, not a backoff step later.
func TestRecover_AReplugEndsAReadingPumpsBackoff(t *testing.T) {
	prev := shareRetryBackoff
	shareRetryBackoff = []time.Duration{time.Hour}
	t.Cleanup(func() { shareRetryBackoff = prev }) // after the harness's Shutdown
	h := newCSHarness(t, csBrio())
	fp := installFakeProducers(h.s)
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	h.watch()
	h.unplug("video0")

	node := h.openedNodes()[0]
	node.send(t, 1) // the app reads while its camera is away
	h.waitLogged("retrying in 1h0m0s", 1)
	h.plug(csBrio())
	if !csWaitFor(2*time.Second, func() bool { return fp.count() == 1 }) {
		t.Fatalf("the camera did not start within 2 s of coming back (producers %d); logs: %v", fp.count(), h.logs.All())
	}
	if got := h.reportOf("/dev/video0")["share_state"]; got != "cv-a (reading)" {
		t.Fatalf("share_state = %q, want cv-a (reading)", got)
	}
}

// M3: the GStreamer element listing behind every share decision is made when
// discovery starts, so the first camera app's create does not spend its budget
// on it.
func TestStartDiscovery_ListsGStreamerElementsAhead(t *testing.T) {
	prev := gstDiscover
	listed := make(chan struct{}, 4)
	gstDiscover = func() (string, map[string]bool, error) {
		listed <- struct{}{}
		return "gst", map[string]bool{"jpegdec": true}, nil
	}
	t.Cleanup(func() { gstDiscover = prev }) // after the harness's Shutdown
	h := newCSHarness(t, csBrio())
	h.s.gstMemo.mu.Lock()
	h.s.gstMemo.gstPath, h.s.gstMemo.available = "", nil
	h.s.gstMemo.mu.Unlock()

	h.watch()
	select {
	case <-listed:
	case <-time.After(2 * time.Second):
		t.Fatal("discovery started without listing GStreamer elements")
	}
	if !csWaitFor(time.Second, func() bool {
		h.s.gstMemo.mu.Lock()
		defer h.s.gstMemo.mu.Unlock()
		return h.s.gstMemo.available != nil
	}) {
		t.Fatal("the listing was not kept")
	}
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	if n := len(h.records("cv-a")); n != 1 || len(listed) != 0 {
		t.Fatalf("records %d, further listings %d; want the share made from the kept listing", n, len(listed))
	}
}

// M-wd: when the /dev watch loses events, every held namespace's covers are
// checked and made again where they are gone.
func TestRecover_AWatchOverflowChecksEveryCover(t *testing.T) {
	h := newCSHarness(t, csBrio())
	h.s.CreateCameraShares(context.Background(), "cv-a", "sh.wendy.cv-a")
	if errs := h.s.CoverCameraShares(context.Background(), "cv-a", 11); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	h.watch()
	time.Sleep(10 * cameraShareWatchSettle) // the watch's own first check has run
	h.setCovered(false)                     // a replug the lost events hid
	h.feedOverflow()
	h.waitLogged("camera share: the /dev watch lost events (its inotify queue overflowed); checking every shared camera's cover", 1)
	h.waitLogged("camera share: covered /dev/video0 for cv-a again; it was replugged while /dev was not watched", 1)
	if covers := h.coverList(); len(covers) != 2 || covers[1] != (csCover{11, "/dev/video255", "/dev/video0", cameraShareRecoverTimeout}) {
		t.Fatalf("covers = %+v, want the start's and one again", covers)
	}
}
