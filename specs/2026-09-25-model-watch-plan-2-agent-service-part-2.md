# Model Watch, Plan 2: Agent Model Service Implementation Plan (part 2 of 2)

> **For agentic workers:** this continues `specs/2026-09-25-model-watch-plan-2-agent-service.md`, which holds the goal, architecture, global constraints, review focus, file structure and Tasks 1–8. Read it first. The plan is split in two only to keep each file within the API review's per-file size limit.

### Task 9: models: restarts, stalls, engine builds and orphan cleanup

**Files:**
- Modify: `go/internal/agent/models/instance.go` (add `building`, `holdsBuild`)
- Modify: `go/internal/agent/models/supervisor.go` (constants; `build` slot on `Supervisor`)
- Modify: `go/internal/agent/models/lifecycle.go` (replace `run`, `runHost`, `handleStatus`, `finish`; extend `prepare`; add `checkHost`, `sleep`, `armStallLocked`, `releaseBuildLocked`, `CleanupOrphans`)
- Test: `go/internal/agent/models/failures_test.go` (package `models_test`)

**Interfaces:**
- Consumes: Tasks 6–8.
- Produces: `(*Supervisor) CleanupOrphans(ctx) error`. Behaviour:
  - crashes and stalls restart with 2/8/30 s backoff, and the fourth failure is final;
  - a host-reported failure is final;
  - one engine build runs at a time, with a 15 min timeout;
  - statuses from a host being replaced are ignored.

- [ ] **Step 1: Write the failing tests**

`go/internal/agent/models/failures_test.go`:

```go
package models_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/models/modelstest"
)

func TestCrashedHostRestartsAfterBackoff(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	if _, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID}); err != nil {
		t.Fatal(err)
	}
	h.runtime.Exit(info.ID, 1)
	modelstest.Eventually(t, "restarting", func() bool { return h.info(info.ID).State == models.StateRestarting })
	if detail := h.info(info.ID).StateDetail; !strings.Contains(detail, "status 1") {
		t.Fatalf("detail = %q", detail)
	}
	h.clock.Advance(time.Second)
	time.Sleep(10 * time.Millisecond)
	if h.runtime.Starts(info.ID) != 1 {
		t.Fatal("restarted before the 2 s backoff")
	}
	modelstest.AdvanceUntil(t, h.clock, 500*time.Millisecond, "the restart", func() bool { return h.runtime.Starts(info.ID) == 2 })
	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Status(models.HostReady))
	modelstest.Eventually(t, "ready again", func() bool { return h.info(info.ID).State == models.StateReady })
}

func TestFourthFailureIsFinal(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	for start := 1; start <= 3; start++ {
		h.runtime.Exit(info.ID, 1)
		modelstest.AdvanceUntil(t, h.clock, time.Second, "a restart", func() bool { return h.runtime.Starts(info.ID) == start+1 })
	}
	h.runtime.Exit(info.ID, 1)
	final := drainToEnd(t, w)
	if final == nil || final.State != models.StateFailed || !strings.Contains(final.StateDetail, "host failed 4 times") {
		t.Fatalf("final = %+v", final)
	}
}

func TestReportedFailureIsFinal(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Failed("no frames from camera"))
	final := drainToEnd(t, w)
	if final == nil || final.State != models.StateFailed || final.StateDetail != "no frames from camera" {
		t.Fatalf("final = %+v", final)
	}
	if n := h.runtime.Starts(info.ID); n != 1 {
		t.Fatalf("a reported failure was retried: %d starts", n)
	}
}

func TestSilentHostIsRestarted(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info, _, err := h.sup.Start(context.Background(), "coco-detector", frontDoor)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID}); err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the host to start", func() bool { return h.runtime.Running(info.ID) })
	modelstest.AdvanceUntil(t, h.clock, time.Second, "the silent host to be replaced", func() bool { return h.runtime.Starts(info.ID) == 2 })
	if !slices.Contains(h.runtime.Removed(), info.ID) {
		t.Fatal("the silent host was not removed before its restart")
	}
}

func TestStaleStatusDuringRestartIsIgnored(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	if _, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID}); err != nil {
		t.Fatal(err)
	}
	h.runtime.Exit(info.ID, 1)
	modelstest.Eventually(t, "restarting", func() bool { return h.info(info.ID).State == models.StateRestarting })
	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Status(models.HostReady))
	time.Sleep(20 * time.Millisecond)
	if st := h.info(info.ID).State; st != models.StateRestarting {
		t.Fatalf("a status from the dead host moved the instance to %v", st)
	}
}

func TestEngineBuildsRunOneAtATime(t *testing.T) {
	h := newHarness(t, models.EngineTensorRT)
	ctx := context.Background()
	first, _, err := h.sup.Start(ctx, "coco-detector", frontDoor)
	if err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the first host", func() bool { return h.runtime.Running(first.ID) })
	if h.runtime.LastSpec().EngineCache == "" {
		t.Fatal("a TensorRT host got no engine cache")
	}
	second, _, err := h.sup.Start(ctx, "coco-detector", garage)
	if err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the second to wait", func() bool {
		return h.info(second.ID).StateDetail == "waiting for another engine build"
	})
	h.sup.PublishApplicationRecord(models.AppIDPrefix+first.ID, modelstest.Status(models.HostBuildingEngine))
	modelstest.Eventually(t, "building", func() bool { return h.info(first.ID).StateDetail == "building TensorRT engine" })
	if h.runtime.Running(second.ID) {
		t.Fatal("two engine builds ran at once")
	}
	h.sup.PublishApplicationRecord(models.AppIDPrefix+first.ID, modelstest.Status(models.HostReady))
	modelstest.Eventually(t, "the second host to start", func() bool { return h.runtime.Running(second.ID) })
}

func TestEngineBuildTimesOut(t *testing.T) {
	h := newHarness(t, models.EngineTensorRT)
	info, _, err := h.sup.Start(context.Background(), "coco-detector", frontDoor)
	if err != nil {
		t.Fatal(err)
	}
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the host", func() bool { return h.runtime.Running(info.ID) })
	app := models.AppIDPrefix + info.ID
	for i := 0; i < 200 && h.info(info.ID).State != 0; i++ {
		h.sup.PublishApplicationRecord(app, modelstest.Status(models.HostBuildingEngine))
		h.clock.Advance(5 * time.Second)
		time.Sleep(time.Millisecond)
	}
	final := drainToEnd(t, w)
	if final == nil || final.State != models.StateFailed || !strings.Contains(final.StateDetail, "engine build took longer than 15 min") {
		t.Fatalf("final = %+v", final)
	}
}

func TestCleanupOrphansRemovesLeftovers(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	h.runtime.AddLeftover("m-0ld00001")
	stale := filepath.Join(h.root, "run", "m-0ld00001")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := h.sup.CleanupOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.runtime.Removed(), "m-0ld00001") {
		t.Fatal("the leftover host was not removed")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the leftover run directory remains: %v", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/agent/models/... -run 'TestCrashed|TestFourth|TestReportedFailure|TestSilent|TestStale|TestEngineBuild|TestCleanupOrphans'`
Expected: FAIL. `CleanupOrphans` is undefined, and crashes end the instance instead of restarting it.

- [ ] **Step 3: Add the new state**

In `go/internal/agent/models/instance.go`, add after the `ring` field:

```go
	building   time.Time // first building_engine report from the current host; zero otherwise
	holdsBuild bool      // this instance holds the device's engine build slot
```

In `go/internal/agent/models/supervisor.go`:

1. Extend the constant block with:

```go
	stallTimeout       = 15 * time.Second
	engineBuildTimeout = 15 * time.Minute
	maxRestarts        = 3
```

and add below it:

```go
// restartBackoff is how long to wait before the first, second and third restart.
var restartBackoff = [maxRestarts]time.Duration{2 * time.Second, 8 * time.Second, 30 * time.Second}
```

2. Add a field to `Supervisor`, above `mu`:

```go
	build chan struct{} // the device's single engine build slot
```

3. In `NewSupervisor`, add `build: make(chan struct{}, 1),` to the returned literal.

- [ ] **Step 4: Replace the run loop**

In `go/internal/agent/models/lifecycle.go`:

1. Add `"time"` to the imports.
2. In `prepare`, add this directly before the final `inst.mu.Lock()` / `inst.modelFile = file` block:

```go
	if s.needsEngineBuild(inst.variant) {
		select {
		case s.build <- struct{}{}:
		default:
			s.setState(inst, StatePreparing, "waiting for another engine build")
			select {
			case s.build <- struct{}{}:
			case <-inst.ctx.Done():
				return inst.ctx.Err()
			}
		}
		inst.mu.Lock()
		inst.holdsBuild = true
		inst.mu.Unlock()
	}
```

3. Replace `run` and `runHost` with:

```go
type hostOutcome int

const (
	hostStopped     hostOutcome = iota // the instance was asked to stop
	hostFailedFinal                    // reported failure or expired engine build
	hostCrashed                        // exited or went silent; worth a restart
)

const engineBuildTimeoutReason = "engine build took longer than 15 min"

// run takes an instance from preparation to removal, restarting a crashed or
// silent host up to maxRestarts times.
func (s *Supervisor) run(inst *instance) {
	final, detail := StateStopped, ""
	defer func() { s.finish(inst, final, detail) }()
	if err := s.prepare(inst); err != nil {
		if inst.ctx.Err() == nil {
			final, detail = StateFailed, err.Error()
		}
		return
	}
	for attempt := 0; ; attempt++ {
		outcome, reason := s.runHost(inst)
		switch outcome {
		case hostStopped:
			return
		case hostFailedFinal:
			final, detail = StateFailed, reason
			return
		}
		if attempt == maxRestarts {
			final, detail = StateFailed, fmt.Sprintf("host failed %d times; last: %s", attempt+1, reason)
			return
		}
		s.setState(inst, StateRestarting, reason)
		s.removeHost(inst)
		if !s.sleep(inst, restartBackoff[attempt]) {
			return
		}
	}
}

// runHost starts one host and waits until the instance must stop, the host
// reports a failure, or it crashes or goes silent.
func (s *Supervisor) runHost(inst *instance) (hostOutcome, string) {
	// Mark the host live before it starts, so an immediate "ready" counts.
	inst.mu.Lock()
	inst.hostRunning, inst.hostFailure, inst.building = true, "", time.Time{}
	inst.lastStatus = s.clock.Now()
	inst.setStateLocked(StateStarting, "")
	s.armStallLocked(inst)
	inst.mu.Unlock()
	defer func() {
		inst.mu.Lock()
		inst.hostRunning = false
		inst.mu.Unlock()
	}()

	exits, err := s.cfg.Runtime.StartHost(inst.ctx, s.hostSpec(inst))
	if err != nil {
		if inst.ctx.Err() != nil {
			return hostStopped, ""
		}
		return hostCrashed, "starting host: " + err.Error()
	}
	for {
		select {
		case <-inst.ctx.Done():
			return hostStopped, ""
		case exit := <-exits:
			if failure := inst.failure(); failure != "" {
				return hostFailedFinal, failure
			}
			return hostCrashed, fmt.Sprintf("host exited with status %d", exit.Code) + s.logTail(inst)
		case <-inst.wake:
			outcome, reason, done := s.checkHost(inst)
			if !done {
				continue
			}
			if reason == engineBuildTimeoutReason {
				reason += s.logTail(inst)
			}
			return outcome, reason
		}
	}
}

// checkHost looks for a reported failure, an expired engine build, or a
// silent host.
func (s *Supervisor) checkHost(inst *instance) (hostOutcome, string, bool) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	now := s.clock.Now()
	switch {
	case inst.hostFailure != "":
		return hostFailedFinal, inst.hostFailure, true
	case !inst.building.IsZero() && inst.state != StateReady && now.Sub(inst.building) >= engineBuildTimeout:
		return hostFailedFinal, engineBuildTimeoutReason, true
	case now.Sub(inst.lastStatus) >= stallTimeout:
		return hostCrashed, "host sent no status for 15 s", true
	}
	return 0, "", false
}

// sleep waits d on the supervisor's clock; false means the instance was
// stopped first.
func (s *Supervisor) sleep(inst *instance, d time.Duration) bool {
	fired := make(chan struct{})
	t := s.clock.AfterFunc(d, func() { close(fired) })
	defer t.Stop()
	select {
	case <-fired:
		return true
	case <-inst.ctx.Done():
		return false
	}
}

// armStallLocked wakes the run loop once the host has been silent for
// stallTimeout; every status re-arms it. Caller holds inst.mu.
func (s *Supervisor) armStallLocked(inst *instance) {
	s.clock.AfterFunc(stallTimeout, inst.poke)
}

// releaseBuildLocked frees the engine build slot if this instance holds it.
// Caller holds inst.mu.
func (s *Supervisor) releaseBuildLocked(inst *instance) {
	if inst.holdsBuild {
		inst.holdsBuild = false
		<-s.build
	}
}
```

4. Replace `handleStatus` with:

```go
// handleStatus applies a model.status record from the live host.
func (s *Supervisor) handleStatus(inst *instance, st HostStatus) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if !inst.hostRunning {
		return // a host being replaced or removed no longer speaks for the instance
	}
	now := s.clock.Now()
	inst.lastStatus = now
	inst.stats = st.Stats
	s.armStallLocked(inst)
	changed := false
	switch st.State {
	case HostFailed:
		inst.hostFailure = st.Reason
		if inst.hostFailure == "" {
			inst.hostFailure = "the model host reported a failure"
		}
		inst.poke()
		return
	case HostBuildingEngine:
		if inst.building.IsZero() {
			inst.building = now
			s.clock.AfterFunc(engineBuildTimeout, inst.poke)
		}
		changed = inst.setStateLocked(StatePreparing, "building TensorRT engine")
	case HostReady:
		s.releaseBuildLocked(inst)
		changed = inst.setStateLocked(StateReady, "")
	}
	if !changed {
		inst.broadcastLocked() // a heartbeat carries fresh stats
	}
}
```

5. In `finish`, add `s.releaseBuildLocked(inst)` on the line after `s.cancelGraceLocked(inst)`.

6. Append:

```go
// CleanupOrphans removes model hosts and run directories that a previous
// agent process left behind; leases do not survive a restart. Call it before
// serving.
func (s *Supervisor) CleanupOrphans(ctx context.Context) error {
	ids, err := s.cfg.Runtime.ListHosts(ctx)
	if err != nil {
		return fmt.Errorf("listing model hosts: %w", err)
	}
	var errs []error
	for _, id := range ids {
		if err := s.cfg.Runtime.RemoveHost(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("removing model host %s: %w", id, err))
		}
	}
	if err := os.RemoveAll(filepath.Join(s.cfg.Root, "run")); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
```

- [ ] **Step 5: Run the whole package with the race detector, twice**

Run: `go test -race -count=2 ./go/internal/agent/models/...`
Expected: PASS on both runs.

- [ ] **Step 6: Commit**

```bash
git add go/internal/agent/models/
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
models: restart crashed hosts, serialize engine builds, clean up orphans

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 10: Pinned two-plane demand in `VideoService`

Two-plane nodes exist only while something demands them, and the minute-long container sync replaces that demand wholesale. Model hosts carry no app labels, so they need a pin that the sync cannot clear.

**Files:**
- Modify: `go/internal/agent/services/video_service_two_plane.go` (`twoPlaneState` at ~lines 364-380; `SetTwoPlaneContainerConsumers` at ~lines 78-98)
- Create: `go/internal/agent/services/video_service_two_plane_pin.go`
- Test: `go/internal/agent/services/video_service_two_plane_pin_test.go`

**Interfaces:**
- Produces: `(*VideoService) AcquireTwoPlaneNode(ctx, owner, sourceID string) (string, error)` and `(*VideoService) ReleaseTwoPlaneNode(ctx, owner string)`.

- [ ] **Step 1: Write the failing tests**

`go/internal/agent/services/video_service_two_plane_pin_test.go`:

```go
package services

import (
	"context"
	"testing"
)

func TestTwoPlanePinSurvivesContainerSync(t *testing.T) {
	video, loop, _ := newTwoPlaneTestService(t)
	ctx := context.Background()
	node, err := video.AcquireTwoPlaneNode(ctx, "m-1", "v4l2:/dev/video0")
	if err != nil {
		t.Fatal(err)
	}
	if path, ok := video.TwoPlaneNodePath("v4l2:/dev/video0"); !ok || path != node {
		t.Fatalf("node %q is not the published one (%q, %v)", node, path, ok)
	}
	// The minute sweep finds no camera-entitled app containers.
	video.SetTwoPlaneContainerConsumers(ctx, nil)
	if _, ok := video.TwoPlaneNodePath("v4l2:/dev/video0"); !ok {
		t.Fatal("a container sync tore down the node a model host holds")
	}
	video.ReleaseTwoPlaneNode(ctx, "m-1")
	waitUntilTwoPlane(t, "the node to go once the pin is released", func() bool {
		_, ok := video.TwoPlaneNodePath("v4l2:/dev/video0")
		return !ok
	})
	if got := loop.auxCreatedCount(); got != 1 {
		t.Fatalf("created %d nodes, want 1", got)
	}
}

func TestTwoPlaneAcquireUnknownSourceLeavesNoPin(t *testing.T) {
	video, _, _ := newTwoPlaneTestService(t)
	if _, err := video.AcquireTwoPlaneNode(context.Background(), "m-1", "v4l2:/dev/video7"); err == nil {
		t.Fatal("acquired a node for a camera that does not exist")
	}
	video.twoPlaneMu.Lock()
	demand, pins := video.twoPlaneDemand, len(video.twoPlanePinned)
	video.twoPlaneMu.Unlock()
	if demand || pins != 0 {
		t.Fatalf("demand=%v pins=%d after a failed acquire", demand, pins)
	}
}

func TestTwoPlaneContainerDemandSurvivesPinRelease(t *testing.T) {
	video, _, _ := newTwoPlaneTestService(t)
	ctx := context.Background()
	video.SetTwoPlaneContainerConsumers(ctx, []string{"app-a"})
	if _, err := video.AcquireTwoPlaneNode(ctx, "m-1", "v4l2:/dev/video0"); err != nil {
		t.Fatal(err)
	}
	video.ReleaseTwoPlaneNode(ctx, "m-1")
	if _, ok := video.TwoPlaneNodePath("v4l2:/dev/video0"); !ok {
		t.Fatal("releasing a pin stopped the node an app container still uses")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/agent/services/ -run 'TestTwoPlanePin|TestTwoPlaneAcquire|TestTwoPlaneContainerDemand'`
Expected: FAIL to compile with `video.AcquireTwoPlaneNode undefined`.

- [ ] **Step 3: Split demand into containers and pins**

In `go/internal/agent/services/video_service_two_plane.go`, add these fields to `twoPlaneState` next to `twoPlaneDemand`:

```go
	// twoPlaneContainerDemand is the container sync's view: some running app
	// container is entitled to the two-plane path.
	twoPlaneContainerDemand bool
	// twoPlanePinned holds agent-managed owners (model hosts) that need the
	// path whatever the container sync says. twoPlaneDemand is
	// twoPlaneContainerDemand || len(twoPlanePinned) > 0.
	twoPlanePinned map[string]bool
```

In `SetTwoPlaneContainerConsumers`, replace the line `s.twoPlaneDemand = len(containerIDs) > 0` with:

```go
	s.twoPlaneContainerDemand = len(containerIDs) > 0
	s.twoPlaneDemand = s.twoPlaneContainerDemand || len(s.twoPlanePinned) > 0
```

Create `go/internal/agent/services/video_service_two_plane_pin.go`:

```go
package services

import (
	"context"
	"fmt"
)

// AcquireTwoPlaneNode keeps the two-plane data path running for owner, an
// agent-managed model host that carries no app labels and so never counts in
// the container sync, and returns the node carrying sourceID's frames. The
// pin outlasts every container sync until ReleaseTwoPlaneNode. A source with
// no node (unknown, or refused because its stream cannot carry frame
// identity) is an error and leaves no pin behind.
func (s *VideoService) AcquireTwoPlaneNode(ctx context.Context, owner, sourceID string) (string, error) {
	s.twoPlaneMu.Lock()
	if s.twoPlanePinned == nil {
		s.twoPlanePinned = map[string]bool{}
	}
	s.twoPlanePinned[owner] = true
	s.twoPlaneDemand = true
	s.twoPlaneMu.Unlock()

	s.ensureTwoPlaneForLocalCameras(ctx)
	if node, ok := s.TwoPlaneNodePath(sourceID); ok {
		return node, nil
	}
	s.ReleaseTwoPlaneNode(ctx, owner)
	return "", fmt.Errorf("no two-plane node for %s: the camera is missing, or its stream cannot carry frame identity", sourceID)
}

// ReleaseTwoPlaneNode drops owner's pin, and stops the data path when nothing
// else needs it.
func (s *VideoService) ReleaseTwoPlaneNode(_ context.Context, owner string) {
	s.twoPlaneMu.Lock()
	delete(s.twoPlanePinned, owner)
	s.twoPlaneDemand = s.twoPlaneContainerDemand || len(s.twoPlanePinned) > 0
	demand := s.twoPlaneDemand
	s.twoPlaneMu.Unlock()
	if !demand {
		s.stopAllTwoPlane()
	}
}
```

- [ ] **Step 4: Run the new tests and every two-plane test**

Run: `go test -race ./go/internal/agent/services/ -run 'TwoPlane'`
Expected: PASS, including the existing `TestTwoPlaneRefusedSourceIsNotRecreated`.

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/services/video_service_two_plane.go go/internal/agent/services/video_service_two_plane_pin.go go/internal/agent/services/video_service_two_plane_pin_test.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
video: let agent-managed owners pin two-plane demand

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 11: A record sink on the app data socket

**Files:**
- Modify: `go/internal/agent/services/app_data_socket.go` (`AppDataSocketManager` struct ~line 89; `serveConn` ~line 502)
- Test: `go/internal/agent/services/app_data_socket_sink_test.go`

**Interfaces:**
- Produces: `(*AppDataSocketManager) SetRecordSink(func(appID string, rec data.ApplicationRecord))`. The sink sees every record that passes validation, after capture, whether or not capture succeeded.

- [ ] **Step 1: Write the failing test**

`go/internal/agent/services/app_data_socket_sink_test.go`:

```go
package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
)

func TestAppDataSocketForwardsAcceptedRecordsToSink(t *testing.T) {
	capture, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	socketRoot, err := os.MkdirTemp("/tmp", "wendy-sink-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketRoot)
	oldRoot := AppDataSocketRootPath
	AppDataSocketRootPath = socketRoot
	defer func() { AppDataSocketRootPath = oldRoot }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	manager := NewAppDataSocketManager(ctx, nil, capture)
	manager.peerCred = func(net.Conn) (peerCredentials, error) { return peerCredentials{UID: 0, PID: 4242}, nil }
	manager.cgroupOfPID = func(int32) (string, error) {
		return fmt.Sprintf("0::/system.slice/%s-sh.wendy.model.m-1.scope\n", sharedenv.SystemdServiceName()), nil
	}
	type forwarded struct {
		appID string
		rec   data.ApplicationRecord
	}
	got := make(chan forwarded, 4)
	manager.SetRecordSink(func(appID string, rec data.ApplicationRecord) { got <- forwarded{appID, rec} })

	dir, err := manager.Ensure("sh.wendy.model.m-1", "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", filepath.Join(dir, DataSocketFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	send := func(rec data.ApplicationRecord) dataAck {
		body, _ := json.Marshal(rec)
		if err := writeDataFrame(conn, json.RawMessage(body)); err != nil {
			t.Fatal(err)
		}
		ackBody, err := readDataFrame(conn)
		if err != nil {
			t.Fatal(err)
		}
		var ack dataAck
		if err := json.Unmarshal(ackBody, &ack); err != nil {
			t.Fatal(err)
		}
		return ack
	}

	status := data.ApplicationRecord{Version: 1, Type: "event", Name: "model.status",
		Attributes: map[string]any{"state": "ready"}, ClientBootID: "unavailable"}
	if ack := send(status); ack.State == "rejected" {
		t.Fatalf("ack = %+v", ack)
	}
	// A record that fails validation never reaches the sink.
	if ack := send(data.ApplicationRecord{Version: 1, Type: "event", ClientBootID: "unavailable"}); ack.State != "rejected" {
		t.Fatalf("a nameless event was accepted: %+v", ack)
	}
	select {
	case f := <-got:
		if f.appID != "sh.wendy.model.m-1" || f.rec.Name != "model.status" || f.rec.Attributes["state"] != "ready" {
			t.Fatalf("forwarded %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("the accepted record never reached the sink")
	}
	select {
	case f := <-got:
		t.Fatalf("a rejected record reached the sink: %+v", f)
	default:
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./go/internal/agent/services/ -run TestAppDataSocketForwardsAcceptedRecordsToSink`
Expected: FAIL to compile with `manager.SetRecordSink undefined`.

- [ ] **Step 3: Implement the sink**

In `go/internal/agent/services/app_data_socket.go`, add a field to `AppDataSocketManager` after `sockets`:

```go
	// recordSink also receives every record that passes validation; guarded by mu.
	recordSink func(appID string, rec data.ApplicationRecord)
```

Add these methods after `NewAppDataSocketManager`:

```go
// SetRecordSink sends every record the socket accepts to sink as well, after
// capture has seen it. The agent's model supervisor uses it to turn model
// host records into events (internal/agent/models). sink must not block: it
// runs on the connection's read path.
func (m *AppDataSocketManager) SetRecordSink(sink func(appID string, rec data.ApplicationRecord)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordSink = sink
}

func (m *AppDataSocketManager) sink() func(string, data.ApplicationRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recordSink
}
```

In `serveConn`, replace:

```go
		state, err := m.capture.RecordApplication(appID, rec)
		ack := dataAck{Version: 1, State: state}
```

with:

```go
		state, err := m.capture.RecordApplication(appID, rec)
		// A capture failure is about episode storage; live consumers still
		// get the record.
		if sink := m.sink(); sink != nil {
			sink(appID, rec)
		}
		ack := dataAck{Version: 1, State: state}
```

- [ ] **Step 4: Run the new test and the socket suite**

Run: `go test -race ./go/internal/agent/services/ -run 'AppDataSocket|DataProtocol'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/services/app_data_socket.go go/internal/agent/services/app_data_socket_sink_test.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
data socket: forward accepted records to an optional sink

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 12: containerd: the model host runtime

**Files:**
- Create: `go/internal/agent/containerd/model_host.go`
- Test: `go/internal/agent/containerd/model_host_test.go`

**Interfaces:**
- Consumes:
  - `models.HostSpec`, `models.HostExit` and the host path constants (Task 6);
  - `localoci.DefaultSpec`, `ResolveDeviceNode`, `RecordPinnedDevice`, `ApplyEntitlements`, `ApplyOptions{DataSocketDir}` and `DedupeDevices`;
  - `(*Client).applyNvidiaCDI`, `(*Client).applyQualcommNPURuntime`, `(*Client).terminateTask`, `(*Client).UnpackImage`, `c.dataSocketProvider` and `c.withNamespace`.
- Produces: `*containerd.Client` implements `models.Runtime` (`var _ models.Runtime = (*Client)(nil)`).

- [ ] **Step 1: Write the failing tests**

`go/internal/agent/containerd/model_host_test.go`:

```go
package containerd

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
)

func testModelHost() models.HostSpec {
	sha := strings.Repeat("1", 64)
	return models.HostSpec{
		InstanceID: "m-0000beef", AppID: models.AppIDPrefix + "m-0000beef",
		Image:  "ghcr.io/wendylabsinc/wendy-model-host-cpu@sha256:" + strings.Repeat("a", 64),
		Engine: models.EngineONNXRuntime, ModelID: "coco-detector", VariantID: "d-cpu", FileSHA256: sha,
		ModelFile: "/var/lib/wendy/models/files/sha256/" + sha, LabelsFile: "/var/lib/wendy/models/run/m-0000beef/labels.txt",
		CameraNode: "/dev/video255", CameraSource: "v4l2:/dev/video0", LogPath: "/var/lib/wendy/models/run/m-0000beef/host.log",
	}
}

func fakeCameraNode(t *testing.T) {
	t.Helper()
	orig := resolveModelCamera
	t.Cleanup(func() { resolveModelCamera = orig })
	resolveModelCamera = func(path, kind string, follow bool) (int64, int64, error) {
		if path != "/dev/video255" || kind != "c" || follow {
			t.Fatalf("resolved %q %q %v", path, kind, follow)
		}
		return 81, 255, nil
	}
}

var testHostImage = modelHostImage{Args: []string{"/app/host"}, Env: []string{"PATH=/usr/bin", "PYTHONPATH=/app"}, Cwd: "/app"}

func TestModelHostBaseSpecIsLockedDown(t *testing.T) {
	fakeCameraNode(t)
	spec, err := modelHostBaseSpec(testModelHost(), testHostImage)
	if err != nil {
		t.Fatal(err)
	}
	u := spec.Process.User
	if u.UID != models.HostUID || u.GID != models.HostGID || !slices.Contains(u.AdditionalGids, uint32(modelHostVideoGID)) {
		t.Fatalf("user = %+v", u)
	}
	if !spec.Root.Readonly {
		t.Fatal("the root filesystem is writable")
	}
	if c := spec.Process.Capabilities; c == nil || len(c.Bounding)+len(c.Effective)+len(c.Permitted)+len(c.Inheritable)+len(c.Ambient) != 0 {
		t.Fatalf("capabilities = %+v", c)
	}
	if !slices.ContainsFunc(spec.Linux.Namespaces, func(ns localoci.LinuxNamespace) bool { return ns.Type == "network" }) {
		t.Fatal("the host shares the device's network; it must have none")
	}
	if !slices.Equal(spec.Process.Args, []string{"/app/host"}) || spec.Process.Cwd != "/app" {
		t.Fatalf("process = %+v", spec.Process)
	}
	for _, want := range []string{"PYTHONPATH=/app", "WENDY_CAMERA_NODE=/dev/video255", "WENDY_MODEL_FILE=" + models.HostModelFile} {
		if !slices.Contains(spec.Process.Env, want) {
			t.Fatalf("env lacks %q: %v", want, spec.Process.Env)
		}
	}
	var paths []string
	for _, kv := range spec.Process.Env {
		if strings.HasPrefix(kv, "PATH=") {
			paths = append(paths, kv)
		}
	}
	if !slices.Equal(paths, []string{"PATH=/usr/bin"}) {
		t.Fatalf("PATH entries = %v, want only the image's", paths)
	}
	var cameraRules int
	for _, d := range spec.Linux.Resources.Devices {
		if d.Allow && d.Major != nil && *d.Major == 81 {
			cameraRules++
			if d.Minor == nil || *d.Minor != 255 {
				t.Fatalf("the camera rule is not scoped to one node: %+v", d)
			}
		}
	}
	if cameraRules != 1 {
		t.Fatalf("%d camera device rules, want 1", cameraRules)
	}
	mounts := map[string]localoci.Mount{}
	for _, m := range spec.Mounts {
		mounts[m.Destination] = m
	}
	for _, dst := range []string{models.HostModelFile, models.HostLabelsFile} {
		if m, ok := mounts[dst]; !ok || !slices.Contains(m.Options, "ro") {
			t.Fatalf("%s mount = %+v", dst, m)
		}
	}
	if m, ok := mounts["/dev"]; ok && m.Type == "bind" {
		t.Fatal("the device's /dev is bound into a model host")
	}
	if _, ok := mounts[models.HostEngineCacheDir]; ok {
		t.Fatal("an ONNX Runtime host got an engine cache")
	}
	if spec.Linux.CgroupsPath != "" {
		t.Fatal("the base spec must leave the cgroup path to finishModelHostSpec")
	}
}

func TestModelHostTensorRTGetsEngineCache(t *testing.T) {
	fakeCameraNode(t)
	h := testModelHost()
	h.Engine, h.EngineCache = models.EngineTensorRT, "/var/lib/wendy/models/engines/"+h.FileSHA256
	spec, err := modelHostBaseSpec(h, testHostImage)
	if err != nil {
		t.Fatal(err)
	}
	var cache *localoci.Mount
	for i := range spec.Mounts {
		if spec.Mounts[i].Destination == models.HostEngineCacheDir {
			cache = &spec.Mounts[i]
		}
	}
	if cache == nil || cache.Source != h.EngineCache || !slices.Contains(cache.Options, "rw") {
		t.Fatalf("engine cache mount = %+v", cache)
	}
	if !slices.Contains(spec.Process.Env, "WENDY_MODEL_ENGINE_CACHE="+models.HostEngineCacheDir) {
		t.Fatalf("env = %v", spec.Process.Env)
	}
}

func TestFinishModelHostSpecGrantsSocketAndScope(t *testing.T) {
	fakeCameraNode(t)
	dir, err := os.MkdirTemp("/tmp", "wmh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	lis, err := net.Listen("unix", filepath.Join(dir, "data.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	h := testModelHost()
	spec, err := modelHostBaseSpec(h, testHostImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := finishModelHostSpec(spec, h, dir); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(spec.Process.Env, "WENDY_DATA_SOCKET=/run/wendy/data/data.sock") {
		t.Fatalf("env = %v", spec.Process.Env)
	}
	if !slices.Contains(spec.Process.User.AdditionalGids, uint32(2000)) {
		t.Fatal("no data socket group")
	}
	if want := "system.slice:" + sharedenv.SystemdServiceName() + ":" + h.AppID; spec.Linux.CgroupsPath != want {
		t.Fatalf("cgroup = %q, want %q", spec.Linux.CgroupsPath, want)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/agent/containerd/ -run 'TestModelHost|TestFinishModelHostSpec'`
Expected: FAIL to compile with `undefined: modelHostBaseSpec`.

- [ ] **Step 3: Write the runtime**

`go/internal/agent/containerd/model_host.go`:

```go
package containerd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
	"go.uber.org/zap"
)

// Model host containers carry these labels and no sh.wendy/app.* label, so
// app listing, stats, restarts and camera sync never see them (as with the
// ROS 2 inspector). The supervisor owns their lifecycle.
const (
	labelKeyModelInstance = "sh.wendy/model.instance"
	labelKeyModelID       = "sh.wendy/model.id"
	labelKeyModelVariant  = "sh.wendy/model.variant"
	labelKeyModelFile     = "sh.wendy/model.file.sha256"
	modelHostPrefix       = "wendy-model-"
	// modelHostVideoGID opens the camera node, as the camera entitlement's
	// videoGroupGID does.
	modelHostVideoGID = 44
)

var _ models.Runtime = (*Client)(nil)

// resolveModelCamera finds a camera node's device numbers; tests replace it.
var resolveModelCamera = localoci.ResolveDeviceNode

func modelHostName(instanceID string) string { return modelHostPrefix + instanceID }

// HasImage reports whether a model host image is already present.
func (c *Client) HasImage(ctx context.Context, ref string) bool {
	_, err := c.client.GetImage(c.withNamespace(ctx), ref)
	return err == nil
}

// EnsureImage pulls and unpacks a model host image unless it is present.
func (c *Client) EnsureImage(ctx context.Context, ref string) error {
	ctx = c.withNamespace(ctx)
	image, err := c.client.GetImage(ctx, ref)
	if err != nil {
		if image, err = c.client.Pull(ctx, ref, containerd.WithPullUnpack); err != nil {
			return fmt.Errorf("pulling %s: %w", ref, err)
		}
	}
	unpacked, err := image.IsUnpacked(ctx, "")
	if err != nil {
		return fmt.Errorf("checking %s: %w", ref, err)
	}
	if !unpacked {
		if err := c.UnpackImage(ctx, image, nil); err != nil {
			return fmt.Errorf("unpacking %s: %w", ref, err)
		}
	}
	return nil
}

// StartHost creates and starts a model host. The channel receives once,
// when the host's process ends.
func (c *Client) StartHost(ctx context.Context, h models.HostSpec) (<-chan models.HostExit, error) {
	ctx = c.withNamespace(ctx)
	if c.dataSocketProvider == nil {
		return nil, errors.New("app data sockets are unavailable on this agent")
	}
	image, err := c.client.GetImage(ctx, h.Image)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", h.Image, err)
	}
	img, err := imageRunConfig(ctx, image)
	if err != nil {
		return nil, err
	}
	socketDir, err := c.dataSocketProvider.Ensure(h.AppID, "")
	if err != nil {
		return nil, fmt.Errorf("opening the data socket: %w", err)
	}
	release := func() { c.dataSocketProvider.ReleaseApp(h.AppID) }
	spec, err := c.modelHostSpec(h, img, socketDir)
	if err != nil {
		release()
		return nil, err
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		release()
		return nil, err
	}
	name := modelHostName(h.InstanceID)
	ctr, err := c.client.NewContainer(ctx, name,
		containerd.WithImage(image), containerd.WithNewSnapshot(name, image),
		containerd.WithContainerLabels(map[string]string{
			labelKeyModelInstance: h.InstanceID, labelKeyModelID: h.ModelID,
			labelKeyModelVariant: h.VariantID, labelKeyModelFile: h.FileSHA256,
		}),
		containerd.WithNewSpec(oci.WithSpecFromBytes(specJSON)),
	)
	if err != nil {
		release()
		return nil, fmt.Errorf("creating the model host: %w", err)
	}
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = ctr.Delete(cleanupCtx, containerd.WithSnapshotCleanup)
		release()
	}
	task, err := ctr.NewTask(ctx, cio.LogFile(h.LogPath))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("creating the model host task: %w", err)
	}
	// Wait before Start so an immediate exit is not missed; it outlives ctx.
	statusC, err := task.Wait(context.WithoutCancel(ctx))
	if err != nil {
		_, _ = task.Delete(context.WithoutCancel(ctx), containerd.WithProcessKill)
		cleanup()
		return nil, fmt.Errorf("waiting on the model host: %w", err)
	}
	if err := task.Start(ctx); err != nil {
		_, _ = task.Delete(context.WithoutCancel(ctx), containerd.WithProcessKill)
		cleanup()
		return nil, fmt.Errorf("starting the model host: %w", err)
	}
	exits := make(chan models.HostExit, 1)
	go func() {
		st := <-statusC
		code, _, err := st.Result()
		exits <- models.HostExit{Code: code, Err: err}
	}()
	return exits, nil
}

// RemoveHost stops and deletes a model host and releases its data socket.
func (c *Client) RemoveHost(ctx context.Context, instanceID string) error {
	ctx = c.withNamespace(ctx)
	defer func() {
		if c.dataSocketProvider != nil {
			c.dataSocketProvider.ReleaseApp(models.AppIDPrefix + instanceID)
		}
	}()
	ctr, err := c.client.LoadContainer(ctx, modelHostName(instanceID))
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if task, err := ctr.Task(ctx, nil); err == nil {
		if err := c.terminateTask(ctx, task, ctr.ID(), syscall.SIGTERM, stopGracePeriod, killWaitTimeout); err != nil {
			c.logger.Warn("stopping a model host failed", zap.String("instance", instanceID), zap.Error(err))
		}
	}
	if err := ctr.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("deleting the model host: %w", err)
	}
	return nil
}

// ListHosts returns the instance ids of every model host container.
func (c *Client) ListHosts(ctx context.Context) ([]string, error) {
	ctx = c.withNamespace(ctx)
	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyModelInstance))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, ctr := range ctrs {
		labels, err := ctr.Labels(ctx)
		if err != nil {
			continue
		}
		if id := labels[labelKeyModelInstance]; id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// modelHostImage is the part of an image's config a model host inherits.
type modelHostImage struct {
	Args []string
	Env  []string
	Cwd  string
}

func imageRunConfig(ctx context.Context, image containerd.Image) (modelHostImage, error) {
	spec, err := image.Spec(ctx)
	if err != nil {
		return modelHostImage{}, fmt.Errorf("reading the model host image config: %w", err)
	}
	args := append(append([]string{}, spec.Config.Entrypoint...), spec.Config.Cmd...)
	if len(args) == 0 {
		return modelHostImage{}, errors.New("the model host image has no entrypoint or command")
	}
	cwd := spec.Config.WorkingDir
	if cwd == "" {
		cwd = "/"
	}
	return modelHostImage{Args: args, Env: spec.Config.Env, Cwd: cwd}, nil
}

// modelHostSpec is the full spec: the locked-down base, the engine's
// accelerator runtime, then the grants and cgroup scope.
func (c *Client) modelHostSpec(h models.HostSpec, img modelHostImage, socketDir string) (*localoci.Spec, error) {
	spec, err := modelHostBaseSpec(h, img)
	if err != nil {
		return nil, err
	}
	switch h.Engine {
	case models.EngineTensorRT:
		if err := c.applyNvidiaCDI(spec); err != nil {
			return nil, fmt.Errorf("adding the NVIDIA runtime: %w", err)
		}
	case models.EngineQNN:
		c.applyQualcommNPURuntime(spec)
	}
	if err := finishModelHostSpec(spec, h, socketDir); err != nil {
		return nil, err
	}
	return spec, nil
}

// modelHostBaseSpec runs a model host as an unprivileged user with a
// read-only root, no capabilities, and a network namespace of its own that no
// CNI configures, so it has no network: the agent fetches everything the host
// needs. The host sees exactly one camera node, not the whole /dev that the
// camera entitlement grants.
func modelHostBaseSpec(h models.HostSpec, img modelHostImage) (*localoci.Spec, error) {
	spec := localoci.DefaultSpec("rootfs", img.Args)
	spec.Process.Cwd = img.Cwd
	spec.Process.User = localoci.User{UID: models.HostUID, GID: models.HostGID, AdditionalGids: []uint32{modelHostVideoGID}}
	spec.Process.Capabilities = &localoci.LinuxCapabilities{}
	spec.Root.Readonly = true
	spec.Process.Env = mergeEnv(spec.Process.Env, img.Env, h.Env())
	ro := []string{"bind", "ro", "nosuid", "nodev", "noexec"}
	spec.Mounts = append(spec.Mounts,
		localoci.Mount{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777", "size=256m"}},
		localoci.Mount{Destination: models.HostModelFile, Type: "bind", Source: h.ModelFile, Options: ro},
		localoci.Mount{Destination: models.HostLabelsFile, Type: "bind", Source: h.LabelsFile, Options: ro},
	)
	if h.EngineCache != "" {
		spec.Mounts = append(spec.Mounts, localoci.Mount{Destination: models.HostEngineCacheDir, Type: "bind",
			Source: h.EngineCache, Options: []string{"bind", "rw", "nosuid", "nodev", "noexec"}})
	}
	major, minor, err := resolveModelCamera(h.CameraNode, "c", false)
	if err != nil {
		return nil, fmt.Errorf("camera node %s: %w", h.CameraNode, err)
	}
	spec.Mounts = append(spec.Mounts, localoci.Mount{Destination: h.CameraNode, Source: h.CameraNode, Type: "bind",
		Options: []string{"bind", "rw", "nosuid", "noexec"}})
	spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices,
		localoci.LinuxDeviceCgroup{Allow: true, Type: "c", Major: &major, Minor: &minor, Access: "rw"})
	localoci.RecordPinnedDevice(spec, h.CameraNode, "c", major, minor)
	return spec, nil
}

// mergeEnv combines environments. A later entry replaces an earlier one with
// the same key: a process reads the first match, so a plain append would let
// the default PATH shadow the image's, and the image could shadow the host
// contract.
func mergeEnv(lists ...[]string) []string {
	index := map[string]int{}
	var out []string
	for _, list := range lists {
		for _, kv := range list {
			key, _, _ := strings.Cut(kv, "=")
			if i, ok := index[key]; ok {
				out[i] = kv
				continue
			}
			index[key] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

// finishModelHostSpec grants the data socket and the engine's accelerator
// through the same entitlement code apps use, then sets the cgroup scope that
// the data socket's peer check attributes to h.AppID.
func finishModelHostSpec(spec *localoci.Spec, h models.HostSpec, socketDir string) error {
	ents := []appconfig.Entitlement{{Type: appconfig.EntitlementEpisodeWrite}}
	switch h.Engine {
	case models.EngineTensorRT:
		ents = append(ents, appconfig.Entitlement{Type: appconfig.EntitlementGPU})
	case models.EngineQNN:
		ents = append(ents, appconfig.Entitlement{Type: appconfig.EntitlementNPU})
	}
	cfg := &appconfig.AppConfig{AppID: h.AppID, Entitlements: ents}
	if err := localoci.ApplyEntitlements(spec, cfg, localoci.ApplyOptions{DataSocketDir: socketDir}); err != nil {
		return fmt.Errorf("granting model host access: %w", err)
	}
	if spec.Linux.CgroupsPath != "" {
		return fmt.Errorf("security: CgroupsPath was set before assignment (%q)", spec.Linux.CgroupsPath)
	}
	spec.Linux.CgroupsPath = fmt.Sprintf("system.slice:%s:%s", sharedenv.SystemdServiceName(), h.AppID)
	localoci.DedupeDevices(spec)
	return nil
}
```

- [ ] **Step 4: Run the tests and the package**

Run: `go test ./go/internal/agent/containerd/ -run 'TestModelHost|TestFinishModelHostSpec'`
Expected: PASS.
Run: `go vet ./go/internal/agent/containerd/ && go test ./go/internal/agent/containerd/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/containerd/model_host.go go/internal/agent/containerd/model_host_test.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
containerd: run locked-down model host containers

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 13: services: `ModelService` gRPC handlers and adapters

**Files:**
- Create: `go/internal/agent/services/model_service.go`
- Create: `go/internal/agent/services/model_adapters.go`
- Test: `go/internal/agent/services/model_service_test.go`

**Interfaces:**
- Consumes:
  - `models.Supervisor` (Tasks 6–9) and `modelstest` fakes;
  - the generated `agentpbv2` types (Task 2);
  - `fakeServerStream[T]` (existing, `ros2_service_test.go:426`);
  - `(*VideoService).AcquireTwoPlaneNode` and `ReleaseTwoPlaneNode` (Task 10);
  - `detectGPUInfo`, `detectNPUInfo` and `gpudiscovery.Summary` (existing).
- Produces:
  - `NewModelService(*zap.Logger, *models.Supervisor) *ModelService`;
  - `ModelDeviceProfile() models.DeviceProfile`;
  - `ModelCameras{Video *VideoService; Data *data.Manager}`, which implements `models.Cameras`.

- [ ] **Step 1: Write the failing tests**

`go/internal/agent/services/model_service_test.go`:

```go
package services

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/models/modelstest"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newTestModelService(t *testing.T) (*ModelService, *modelstest.Runtime) {
	t.Helper()
	rt := modelstest.NewRuntime()
	sup := models.NewSupervisor(models.Config{
		Catalog: modelstest.Catalog(models.EngineONNXRuntime), Device: models.DeviceProfile{Arch: "arm64"},
		Runtime: rt, Cameras: modelstest.NewCameras(models.Camera{SourceID: "v4l2:/dev/video0", Name: "front door"}),
		Files: modelstest.NewFiles(), Root: t.TempDir(), Clock: modelstest.NewClock(),
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sup.Shutdown(ctx)
	})
	return NewModelService(zap.NewNop(), sup), rt
}

func startTestModel(t *testing.T, svc *ModelService, rt *modelstest.Runtime) string {
	t.Helper()
	started, err := svc.StartModel(context.Background(), &agentpbv2.StartModelRequest{ModelId: "coco-detector", CameraSourceId: "v4l2:/dev/video0"})
	if err != nil {
		t.Fatal(err)
	}
	id := started.GetInstance().GetInstanceId()
	modelstest.Eventually(t, "the host to start", func() bool { return rt.Running(id) })
	return id
}

// watchInBackground runs WatchModel and hands every sent message to the test.
func watchInBackground(ctx context.Context, svc *ModelService, req *agentpbv2.WatchModelRequest) (<-chan *agentpbv2.ModelWatchMessage, <-chan error) {
	sent := make(chan *agentpbv2.ModelWatchMessage, 64)
	stream := &fakeServerStream[agentpbv2.ModelWatchMessage]{ctx: ctx}
	stream.onSend = func(n int) error { sent <- stream.sent[n-1]; return nil }
	done := make(chan error, 1)
	go func() { done <- svc.WatchModel(req, stream) }()
	return sent, done
}

func receive(t *testing.T, sent <-chan *agentpbv2.ModelWatchMessage, match func(*agentpbv2.ModelWatchMessage) bool) *agentpbv2.ModelWatchMessage {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-sent:
			if match(msg) {
				return msg
			}
		case <-deadline:
			t.Fatal("the expected message never arrived")
		}
	}
}

func TestModelServiceCatalogStartAndList(t *testing.T) {
	svc, rt := newTestModelService(t)
	ctx := context.Background()
	cat, err := svc.ListCatalog(ctx, &agentpbv2.ListModelCatalogRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.GetModels()) != 1 || cat.GetModels()[0].GetVariant().GetEngine() != models.EngineONNXRuntime ||
		len(cat.GetCameras()) != 1 || cat.GetMaxRunning() != 2 || cat.GetModels()[0].GetVariant().GetDownloadBytes() != 10 {
		t.Fatalf("catalog = %v", cat)
	}
	id := startTestModel(t, svc, rt)
	list, err := svc.ListModels(ctx, &agentpbv2.ListModelsRequest{})
	if err != nil || len(list.GetInstances()) != 1 || list.GetInstances()[0].GetInstanceId() != id {
		t.Fatalf("list = %v, %v", list, err)
	}
	if list.GetInstances()[0].GetStartedUnixNanos() == 0 {
		t.Fatal("the start time is not reported")
	}
}

func TestModelServiceMapsErrorsToCodes(t *testing.T) {
	svc, _ := newTestModelService(t)
	ctx := context.Background()
	cases := []struct {
		name string
		call func() error
		want codes.Code
	}{
		{"unknown model", func() error {
			_, err := svc.StartModel(ctx, &agentpbv2.StartModelRequest{ModelId: "nope", CameraSourceId: "v4l2:/dev/video0"})
			return err
		}, codes.NotFound},
		{"unknown camera", func() error {
			_, err := svc.StartModel(ctx, &agentpbv2.StartModelRequest{ModelId: "coco-detector", CameraSourceId: "v4l2:/dev/video9"})
			return err
		}, codes.NotFound},
		{"unknown instance", func() error {
			_, err := svc.StopModel(ctx, &agentpbv2.StopModelRequest{InstanceId: "m-missing"})
			return err
		}, codes.NotFound},
	}
	for _, tc := range cases {
		if got := status.Code(tc.call()); got != tc.want {
			t.Errorf("%s: code %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWatchModelStreamsEventsAndDetachesOnDisconnect(t *testing.T) {
	svc, rt := newTestModelService(t)
	id := startTestModel(t, svc, rt)
	ctx, cancel := context.WithCancel(context.Background())
	sent, done := watchInBackground(ctx, svc, &agentpbv2.WatchModelRequest{InstanceId: id, Classes: []string{"person"}})

	started := receive(t, sent, func(m *agentpbv2.ModelWatchMessage) bool { return m.GetStarted() != nil })
	if started.GetStarted().GetWatchId() == "" || started.GetStarted().GetInstance().GetInstanceId() != id {
		t.Fatalf("started = %v", started)
	}
	svc.supervisor.PublishApplicationRecord(models.AppIDPrefix+id, modelstest.Entered("car", 0.9, 1))
	svc.supervisor.PublishApplicationRecord(models.AppIDPrefix+id, modelstest.Entered("person", 0.9, 2))
	event := receive(t, sent, func(m *agentpbv2.ModelWatchMessage) bool { return m.GetEvent() != nil }).GetEvent()
	if event.GetClassName() != "person" || event.GetSequence() != 2 || event.GetBox().GetHeight() == 0 {
		t.Fatalf("event = %v", event)
	}

	cancel()
	if err := <-done; status.Code(err) != codes.Canceled {
		t.Fatalf("WatchModel after disconnect = %v", err)
	}
	list, _ := svc.ListModels(context.Background(), &agentpbv2.ListModelsRequest{})
	if got := list.GetInstances()[0].GetWatchers(); got != 0 {
		t.Fatalf("watchers after disconnect = %d; the lost watch must be detached", got)
	}
}

func TestWatchModelEndsWithFinalStatus(t *testing.T) {
	svc, rt := newTestModelService(t)
	id := startTestModel(t, svc, rt)
	sent, done := watchInBackground(context.Background(), svc, &agentpbv2.WatchModelRequest{InstanceId: id})
	receive(t, sent, func(m *agentpbv2.ModelWatchMessage) bool { return m.GetStarted() != nil })
	if _, err := svc.StopModel(context.Background(), &agentpbv2.StopModelRequest{InstanceId: id}); err != nil {
		t.Fatal(err)
	}
	final := receive(t, sent, func(m *agentpbv2.ModelWatchMessage) bool {
		return m.GetStatus().GetState() == agentpbv2.ModelState_MODEL_STATE_STOPPED
	})
	if final.GetStatus().GetStateDetail() != "stopped" {
		t.Fatalf("final = %v", final)
	}
	if err := <-done; err != nil {
		t.Fatalf("WatchModel = %v, want a clean end", err)
	}
}

func TestModelCamerasListsHealthyLocalCameras(t *testing.T) {
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetSourceProvider(func(context.Context) []data.Source {
		return []data.Source{
			{ID: "v4l2:/dev/video0", Kind: "camera", Healthy: true, Detail: "C920 USB"},
			{ID: "ipcamera:200", Kind: "camera", Healthy: true, Detail: "porch"},
			{ID: "v4l2:/dev/video4", Kind: "camera", Healthy: false},
		}
	})
	got := ModelCameras{Data: m}.List(context.Background())
	if want := []models.Camera{{SourceID: "v4l2:/dev/video0", Name: "C920 USB"}}; !slices.Equal(got, want) {
		t.Fatalf("cameras = %+v, want %+v", got, want)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/agent/services/ -run 'TestModelService|TestWatchModel|TestModelCameras'`
Expected: FAIL to compile with `undefined: NewModelService`.

- [ ] **Step 3: Write the service**

`go/internal/agent/services/model_service.go`:

```go
package services

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ModelService serves WendyModelService from the model supervisor.
type ModelService struct {
	agentpbv2.UnimplementedWendyModelServiceServer
	logger     *zap.Logger
	supervisor *models.Supervisor
}

func NewModelService(logger *zap.Logger, supervisor *models.Supervisor) *ModelService {
	return &ModelService{logger: logger, supervisor: supervisor}
}

func modelStatusError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, models.ErrUnknownModel), errors.Is(err, models.ErrUnknownCamera),
		errors.Is(err, models.ErrUnknownInstance), errors.Is(err, models.ErrUnknownWatch):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, models.ErrNoVariant), errors.Is(err, models.ErrCameraNotStreamable):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, models.ErrCapacity):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, models.ErrInvalidFilter):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func (s *ModelService) ListCatalog(ctx context.Context, _ *agentpbv2.ListModelCatalogRequest) (*agentpbv2.ListModelCatalogResponse, error) {
	view := s.supervisor.Catalog(ctx)
	resp := &agentpbv2.ListModelCatalogResponse{
		CatalogVersion: view.Version, MaxRunning: uint32(view.MaxRunning), Running: uint32(view.Running),
	}
	for _, e := range view.Models {
		m := &agentpbv2.CatalogModel{Id: e.Model.ID, Description: e.Model.Description, Kind: e.Model.Kind,
			Labels: e.Model.Labels, UnavailableReason: e.UnavailableReason}
		if e.Variant != nil {
			m.Variant = &agentpbv2.CatalogVariant{Id: e.Variant.ID, Engine: e.Variant.Engine,
				DownloadBytes: uint64(e.DownloadBytes), NeedsEngineBuild: e.NeedsEngineBuild, ImageCached: e.ImageCached}
		}
		resp.Models = append(resp.Models, m)
	}
	for _, c := range view.Cameras {
		resp.Cameras = append(resp.Cameras, &agentpbv2.ModelCamera{SourceId: c.SourceID, Name: c.Name})
	}
	return resp, nil
}

func (s *ModelService) StartModel(ctx context.Context, req *agentpbv2.StartModelRequest) (*agentpbv2.StartModelResponse, error) {
	info, reused, err := s.supervisor.Start(ctx, req.GetModelId(), req.GetCameraSourceId())
	if err != nil {
		return nil, modelStatusError(err)
	}
	return &agentpbv2.StartModelResponse{Instance: modelInstanceProto(info), Reused: reused}, nil
}

// WatchModel holds the instance for as long as the client stays connected.
// A client that goes away is detached, which starts the lease grace period.
func (s *ModelService) WatchModel(req *agentpbv2.WatchModelRequest, stream grpc.ServerStreamingServer[agentpbv2.ModelWatchMessage]) error {
	w, info, last, err := s.supervisor.Watch(models.WatchRequest{
		InstanceID: req.GetInstanceId(), Label: req.GetLabel(), AfterSequence: req.GetAfterSequence(),
		Filter: models.Filter{Classes: req.GetClasses(), MinConfidence: req.GetMinConfidence(), Types: req.GetEventTypes()},
	})
	if err != nil {
		return modelStatusError(err)
	}
	started := &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Started{Started: &agentpbv2.WatchStarted{
		WatchId: w.ID, Instance: modelInstanceProto(info), LastSequence: last}}}
	if err := stream.Send(started); err != nil {
		s.supervisor.Detach(w.InstanceID, w.ID)
		return err
	}
	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			s.supervisor.Detach(w.InstanceID, w.ID)
			return status.FromContextError(ctx.Err()).Err()
		case msg, ok := <-w.C:
			if !ok {
				if final := w.Final(); final != nil {
					return stream.Send(modelStatusMessage(*final))
				}
				return nil // the watch was stopped explicitly
			}
			if err := stream.Send(modelWatchMessage(msg)); err != nil {
				s.supervisor.Detach(w.InstanceID, w.ID)
				return err
			}
		}
	}
}

func (s *ModelService) ListModels(context.Context, *agentpbv2.ListModelsRequest) (*agentpbv2.ListModelsResponse, error) {
	resp := &agentpbv2.ListModelsResponse{}
	for _, info := range s.supervisor.List() {
		resp.Instances = append(resp.Instances, modelInstanceProto(info))
	}
	return resp, nil
}

func (s *ModelService) StopModel(ctx context.Context, req *agentpbv2.StopModelRequest) (*agentpbv2.StopModelResponse, error) {
	info, err := s.supervisor.Stop(ctx, req.GetInstanceId(), req.GetWatchId())
	if err != nil {
		return nil, modelStatusError(err)
	}
	return &agentpbv2.StopModelResponse{Instance: modelInstanceProto(info)}, nil
}

func modelInstanceProto(i models.InstanceInfo) *agentpbv2.ModelInstance {
	return &agentpbv2.ModelInstance{
		InstanceId: i.ID, ModelId: i.ModelID, VariantId: i.VariantID, Engine: i.Engine, CameraSourceId: i.CameraSourceID,
		State: modelStateProto(i.State), StateDetail: i.StateDetail, Watchers: uint32(i.Watchers), WatchLabels: i.WatchLabels,
		StartedUnixNanos: i.StartedAt.UnixNano(), FileSha256: i.FileSHA256,
		Stats: &agentpbv2.ModelStats{ProcessedFps: i.Stats.ProcessedFPS, LatencyP50Ms: i.Stats.LatencyP50Ms, FramesSkipped: i.Stats.FramesSkipped},
	}
}

func modelStateProto(s models.State) agentpbv2.ModelState {
	switch s {
	case models.StatePreparing:
		return agentpbv2.ModelState_MODEL_STATE_PREPARING
	case models.StateStarting:
		return agentpbv2.ModelState_MODEL_STATE_STARTING
	case models.StateReady:
		return agentpbv2.ModelState_MODEL_STATE_READY
	case models.StateRestarting:
		return agentpbv2.ModelState_MODEL_STATE_RESTARTING
	case models.StateFailed:
		return agentpbv2.ModelState_MODEL_STATE_FAILED
	case models.StateStopped:
		return agentpbv2.ModelState_MODEL_STATE_STOPPED
	}
	return agentpbv2.ModelState_MODEL_STATE_UNSPECIFIED
}

func modelStatusMessage(i models.InstanceInfo) *agentpbv2.ModelWatchMessage {
	return &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Status{Status: modelInstanceProto(i)}}
}

func modelWatchMessage(m models.WatchMessage) *agentpbv2.ModelWatchMessage {
	switch {
	case m.Status != nil:
		return modelStatusMessage(*m.Status)
	case m.Event != nil:
		e := m.Event
		return &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Event{Event: &agentpbv2.ModelEvent{
			Sequence: e.Sequence, Type: e.Type, ClassName: e.Class, Confidence: e.Confidence, TrackId: e.TrackID,
			Box:      &agentpbv2.BoundingBox{X: e.Box.X, Y: e.Box.Y, Width: e.Box.Width, Height: e.Box.Height},
			SourceId: e.SourceID, SampleId: e.SampleID, TimeUnixNanos: e.Time.UnixNano(),
		}}}
	default:
		return &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Gap{Gap: &agentpbv2.ModelGap{
			FirstMissing: m.Gap.FirstMissing, LastMissing: m.Gap.LastMissing}}}
	}
}
```

`go/internal/agent/services/model_adapters.go`:

```go
package services

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/gpudiscovery"
	"github.com/wendylabsinc/wendy/go/internal/agent/models"
)

// ModelDeviceProfile describes this device for model variant selection,
// using the same detection device info reports.
func ModelDeviceProfile() models.DeviceProfile {
	gpu := detectGPUInfo()
	npu := detectNPUInfo()
	_, backends := gpudiscovery.Summary(gpu.devices)
	return models.DeviceProfile{Arch: runtime.GOARCH, GPUVendor: gpu.vendor, GPUArch: gpu.gpuArch,
		ComputeBackends: backends, NPUBackends: npu.backends}
}

// ModelCameras serves models.Cameras: local V4L2 cameras from the data
// manager's sources, and their two-plane nodes from the video service.
type ModelCameras struct {
	Video *VideoService
	Data  *data.Manager
}

var _ models.Cameras = ModelCameras{}

func (c ModelCameras) List(ctx context.Context) []models.Camera {
	var out []models.Camera
	for _, src := range c.Data.Sources(ctx) {
		if src.Kind == "camera" && src.Healthy && strings.HasPrefix(src.ID, "v4l2:") {
			out = append(out, models.Camera{SourceID: src.ID, Name: src.Detail})
		}
	}
	return out
}

func (c ModelCameras) Acquire(ctx context.Context, owner, sourceID string) (string, error) {
	node, err := c.Video.AcquireTwoPlaneNode(ctx, owner, sourceID)
	if err != nil {
		return "", fmt.Errorf("%w: %v", models.ErrCameraNotStreamable, err)
	}
	return node, nil
}

func (c ModelCameras) Release(ctx context.Context, owner string) { c.Video.ReleaseTwoPlaneNode(ctx, owner) }
```

- [ ] **Step 4: Run the tests**

Run: `go test -race ./go/internal/agent/services/ -run 'TestModelService|TestWatchModel|TestModelCameras'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/services/model_service.go go/internal/agent/services/model_adapters.go go/internal/agent/services/model_service_test.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
services: serve WendyModelService from the model supervisor

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 14: Wire the model service into the agent

**Files:**
- Create: `go/cmd/wendy-agent/models.go`
- Test: `go/cmd/wendy-agent/models_test.go`
- Modify: `go/cmd/wendy-agent/main.go`: after the camera loopback wiring (~line 392); after the tunnel registration on the mTLS server (~line 787); after `registerAllServices(localSocketServer)` (~line 959)

**Interfaces:**
- Consumes: Tasks 9, 11, 12 and 13.
- Produces: the agent serves `WendyModelService` over mTLS and on the admin socket. `WENDY_MODEL_CATALOG_FILE` overrides the catalog for development.

- [ ] **Step 1: Write the failing tests**

`go/cmd/wendy-agent/models_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestLoadModelCatalogDefaultsToBuiltIn(t *testing.T) {
	c, err := loadModelCatalog(zap.NewNop(), "")
	if err != nil || c.Version == "" {
		t.Fatalf("catalog = %+v, %v", c, err)
	}
}

func TestLoadModelCatalogOverride(t *testing.T) {
	sha := strings.Repeat("1", 64)
	variant := func(image string) string {
		return `{"version":"dev","models":[{"id":"fake-detector","kind":"detector","labels":["person"],"variants":[{"id":"v","engine":"onnxruntime","host_image":"` +
			image + `","file":{"url":"https://example.com/m","sha256":"` + sha + `","bytes":1},"input_size":1}]}]}`
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(good, []byte(variant("ghcr.io/wendylabsinc/wendy-model-host-fake@sha256:"+strings.Repeat("a", 64))), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(variant("ghcr.io/wendylabsinc/wendy-model-host-fake:latest")), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err := loadModelCatalog(zap.NewNop(), good); err != nil || c.Version != "dev" {
		t.Fatalf("override = %+v, %v", c, err)
	}
	if _, err := loadModelCatalog(zap.NewNop(), bad); err == nil {
		t.Fatal("an override with a tag-pinned image was accepted")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/cmd/wendy-agent/ -run TestLoadModelCatalog`
Expected: FAIL to compile with `undefined: loadModelCatalog`.

- [ ] **Step 3: Write the wiring helpers**

`go/cmd/wendy-agent/models.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	agentcontainerd "github.com/wendylabsinc/wendy/go/internal/agent/containerd"
	agentdata "github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentmodels "github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"go.uber.org/zap"
)

// modelRoot holds model files, built engines and per-instance run directories.
const modelRoot = "/var/lib/wendy/models"

// newModelSupervisor builds the model supervisor and removes model hosts a
// previous agent process left running.
func newModelSupervisor(ctx context.Context, logger *zap.Logger, ctrd *agentcontainerd.Client, video *services.VideoService, dataManager *agentdata.Manager) (*agentmodels.Supervisor, error) {
	catalog, err := loadModelCatalog(logger, os.Getenv("WENDY_MODEL_CATALOG_FILE"))
	if err != nil {
		return nil, err
	}
	sup := agentmodels.NewSupervisor(agentmodels.Config{
		Catalog: catalog,
		Device:  services.ModelDeviceProfile(),
		Runtime: ctrd,
		Cameras: services.ModelCameras{Video: video, Data: dataManager},
		Files:   agentmodels.NewFileCache(filepath.Join(modelRoot, "files"), nil),
		Root:    modelRoot,
		Logger:  logger.Named("models"),
	})
	if err := sup.CleanupOrphans(ctx); err != nil {
		logger.Warn("Removing model hosts left by a previous agent failed", zap.Error(err))
	}
	return sup, nil
}

// loadModelCatalog returns the catalog built into the agent, or the file at
// path when it is set. The override exists for development, for example to
// run the fake model host. Only root can set the agent's environment.
func loadModelCatalog(logger *zap.Logger, path string) (agentmodels.Catalog, error) {
	if path == "" {
		return agentmodels.DefaultCatalog()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return agentmodels.Catalog{}, fmt.Errorf("reading WENDY_MODEL_CATALOG_FILE: %w", err)
	}
	logger.Warn("Using a development model catalog", zap.String("path", path))
	return agentmodels.ParseCatalog(raw)
}
```

- [ ] **Step 4: Wire it into `main.go`**

In `go/cmd/wendy-agent/main.go`, directly after this block:

```go
	if ctrdClient != nil {
		ctrdClient.SetCameraLoopbackProvider(videoSvc)
		ctrdClient.SyncCameraLoopbacks(ctx)
		go ctrdClient.RunCameraLoopbackSync(ctx, time.Minute)
	}
```

add:

```go
	// Models the agent runs for clients such as wendy chat
	// (specs/2026-09-25-model-watch-design.md). Model hosts are containers,
	// so the service needs containerd.
	var modelSvc *services.ModelService
	if ctrdClient != nil {
		modelSupervisor, err := newModelSupervisor(ctx, logger, ctrdClient, videoSvc, dataManager)
		if err != nil {
			logger.Error("Model service disabled", zap.Error(err))
		} else {
			appDataSocketManager.SetRecordSink(modelSupervisor.PublishApplicationRecord)
			modelSvc = services.NewModelService(logger, modelSupervisor)
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				modelSupervisor.Shutdown(shutdownCtx)
			}()
		}
	}
```

After `agentpbv2.RegisterWendyTunnelServiceServer(srv, services.NewTunnelService(logger))` in the mTLS server setup, add:

```go
		// WendyModelService runs containers with camera and accelerator access
		// for a client. Like the tunnel, it stays off the plaintext
		// provisioning listener; the admin socket registers it separately.
		if modelSvc != nil {
			agentpbv2.RegisterWendyModelServiceServer(srv, modelSvc)
		}
```

After `registerAllServices(localSocketServer)`, add:

```go
		if modelSvc != nil {
			agentpbv2.RegisterWendyModelServiceServer(localSocketServer, modelSvc)
		}
```

- [ ] **Step 5: Run the tests, then build the agent for both architectures**

Run: `go test ./go/cmd/wendy-agent/ -run TestLoadModelCatalog`
Expected: PASS.
Run: `go vet ./go/cmd/wendy-agent/ && (cd go && make build-agent-linux-arm64 build-agent-linux-amd64)`
Expected: success, producing `go/bin/wendy-agent-linux-arm64` and `go/bin/wendy-agent-linux-amd64`.
Run: `grep -n "RegisterWendyModelServiceServer" go/cmd/wendy-agent/main.go`
Expected: exactly two lines, one under the mTLS server and one under the local socket server. Neither is inside `registerAllServices`.

- [ ] **Step 6: Commit**

```bash
git add go/cmd/wendy-agent/models.go go/cmd/wendy-agent/models_test.go go/cmd/wendy-agent/main.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
agent: serve WendyModelService over mTLS and the admin socket

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 15: CLI: `wendy device model catalog | list | stop`

**Files:**
- Create: `go/internal/cli/commands/device_model.go`
- Test: `go/internal/cli/commands/device_model_test.go`
- Modify: `go/internal/cli/commands/device.go`: add `newDeviceModelCmd(),` to `addToGroup("apps", …)`
- Modify: `go/internal/cli/commands/commands_test.go`: add `"model"` to `expectedSubs` in `TestNewDeviceCmd`

**Interfaces:**
- Consumes: `grpcclient.AgentConnection.ModelService` (Task 2), and the existing helpers `connectToAgent`, `encodeProtoJSON`, `tui.RenderTable`, `jsonOutput`, `startUDSAgent` and `captureCommandStdout`.
- Produces:
  - commands: `newDeviceModelCmd()`, `newDeviceModelCatalogCmd()`, `newDeviceModelListCmd()`, `newDeviceModelStopCmd()`;
  - helpers: `withModelClient(ctx, fn)`, `modelServiceErr(err)`, `modelStateLabel(*agentpbv2.ModelInstance) string` and `modelStateText(*agentpbv2.ModelInstance) string`. Task 16 uses these.

- [ ] **Step 1: Write the failing tests**

`go/internal/cli/commands/device_model_test.go`:

```go
package commands

import (
	"context"
	"strings"
	"sync"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

// fakeModelService records calls; Task 16 extends it with watching.
type fakeModelService struct {
	agentpbv2.UnimplementedWendyModelServiceServer
	catalog *agentpbv2.ListModelCatalogResponse
	list    *agentpbv2.ListModelsResponse
	events  []*agentpbv2.ModelWatchMessage
	hold    bool // keep WatchModel open until the client leaves

	mu      sync.Mutex
	stopped []string // "instance/watch"
	start   *agentpbv2.StartModelRequest
	watch   *agentpbv2.WatchModelRequest
}

func (f *fakeModelService) ListCatalog(context.Context, *agentpbv2.ListModelCatalogRequest) (*agentpbv2.ListModelCatalogResponse, error) {
	return f.catalog, nil
}

func (f *fakeModelService) ListModels(context.Context, *agentpbv2.ListModelsRequest) (*agentpbv2.ListModelsResponse, error) {
	return f.list, nil
}

func (f *fakeModelService) StopModel(_ context.Context, req *agentpbv2.StopModelRequest) (*agentpbv2.StopModelResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, req.GetInstanceId()+"/"+req.GetWatchId())
	return &agentpbv2.StopModelResponse{Instance: &agentpbv2.ModelInstance{
		InstanceId: req.GetInstanceId(), State: agentpbv2.ModelState_MODEL_STATE_STOPPED, StateDetail: "stopped"}}, nil
}

func (f *fakeModelService) stops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stopped...)
}

func serveModels(t *testing.T, fake *fakeModelService) {
	t.Helper()
	startUDSAgent(t, func(srv *grpc.Server) { agentpbv2.RegisterWendyModelServiceServer(srv, fake) })
	old := jsonOutput
	t.Cleanup(func() { jsonOutput = old })
}

func TestDeviceModelCatalogHumanAndJSON(t *testing.T) {
	serveModels(t, &fakeModelService{catalog: &agentpbv2.ListModelCatalogResponse{
		CatalogVersion: "test", MaxRunning: 2,
		Models: []*agentpbv2.CatalogModel{{Id: "coco-detector", Kind: "detector", Labels: []string{"person", "car"},
			Variant: &agentpbv2.CatalogVariant{Id: "d-cpu", Engine: "onnxruntime", DownloadBytes: 12 << 20}}},
		Cameras: []*agentpbv2.ModelCamera{{SourceId: "v4l2:/dev/video0", Name: "C920"}},
	}})
	cmd := newDeviceModelCatalogCmd()
	cmd.SetContext(context.Background())

	jsonOutput = false
	out, err := captureCommandStdout(t, func() error { return cmd.RunE(cmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"coco-detector", "onnxruntime", "2 classes", "downloads its runtime image", "12 MB", "v4l2:/dev/video0", "0 of 2 model slots"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	jsonOutput = true
	out, err = captureCommandStdout(t, func() error { return cmd.RunE(cmd, nil) })
	if err != nil || !strings.Contains(out, `"catalog_version":"test"`) {
		t.Fatalf("json = %s, %v", out, err)
	}
}

func TestDeviceModelListShowsState(t *testing.T) {
	serveModels(t, &fakeModelService{list: &agentpbv2.ListModelsResponse{Instances: []*agentpbv2.ModelInstance{{
		InstanceId: "m-1", ModelId: "coco-detector", CameraSourceId: "v4l2:/dev/video0",
		State: agentpbv2.ModelState_MODEL_STATE_READY, Watchers: 1, FileSha256: strings.Repeat("ab", 32),
		Stats: &agentpbv2.ModelStats{ProcessedFps: 9.8}}}}})
	jsonOutput = false
	cmd := newDeviceModelListCmd()
	cmd.SetContext(context.Background())
	out, err := captureCommandStdout(t, func() error { return cmd.RunE(cmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"m-1", "coco-detector", "ready (9.8 fps)", "abababababab…"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDeviceModelStopStopsOutright(t *testing.T) {
	fake := &fakeModelService{}
	serveModels(t, fake)
	jsonOutput = false
	cmd := newDeviceModelStopCmd()
	cmd.SetContext(context.Background())
	out, err := captureCommandStdout(t, func() error { return cmd.RunE(cmd, []string{"m-1"}) })
	if err != nil {
		t.Fatal(err)
	}
	if got := fake.stops(); len(got) != 1 || got[0] != "m-1/" {
		t.Fatalf("stops = %v, want an outright stop of m-1", got)
	}
	if !strings.Contains(out, "Stopped m-1") {
		t.Fatalf("output = %q", out)
	}
}

func TestDeviceModelExplainsOldAgents(t *testing.T) {
	startUDSAgent(t) // serves no model service
	cmd := newDeviceModelListCmd()
	cmd.SetContext(context.Background())
	_, err := captureCommandStdout(t, func() error { return cmd.RunE(cmd, nil) })
	if err == nil || !strings.Contains(err.Error(), "does not run models yet") {
		t.Fatalf("err = %v", err)
	}
}
```

In `go/internal/cli/commands/commands_test.go` `TestNewDeviceCmd`, change `expectedSubs` to include `"model"`:

```go
	expectedSubs := []string{"info", "version", "set-default", "get-default", "unset-default", "setup", "update", "model"}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/cli/commands/ -run 'TestDeviceModel|TestNewDeviceCmd'`
Expected: FAIL to compile with `undefined: newDeviceModelCatalogCmd`.

- [ ] **Step 3: Write the commands**

`go/internal/cli/commands/device_model.go`:

```go
package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newDeviceModelCmd is `wendy device model`: run catalog models on a device.
func newDeviceModelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "model",
		Short: "Run AI models from the catalog on a device's cameras",
		Long: `Run detectors and other catalog models on a device's cameras.

A model runs while something watches it. 'wendy device model run' watches in
the foreground; after it exits, the device stops the model a minute later
unless another client is watching.`,
	}
	cmd.AddCommand(newDeviceModelCatalogCmd(), newDeviceModelListCmd(), newDeviceModelStopCmd())
	return cmd
}

func withModelClient(ctx context.Context, fn func(agentpbv2.WendyModelServiceClient) error) error {
	conn, err := connectToAgent(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return fn(conn.ModelService)
}

// modelServiceErr explains an agent that predates the model service.
func modelServiceErr(err error) error {
	if status.Code(err) == codes.Unimplemented {
		return errors.New("this device's agent does not run models yet; update it with `wendy device update`")
	}
	return err
}

func newDeviceModelCatalogCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "catalog",
		Short: "List the models and cameras this device can use",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withModelClient(cmd.Context(), func(client agentpbv2.WendyModelServiceClient) error {
				resp, err := client.ListCatalog(cmd.Context(), &agentpbv2.ListModelCatalogRequest{})
				if err != nil {
					return modelServiceErr(err)
				}
				if jsonOutput {
					return encodeProtoJSON(cmd.OutOrStdout(), resp)
				}
				fmt.Fprint(cmd.OutOrStdout(), renderModelCatalog(resp))
				return nil
			})
		},
	}
}

func renderModelCatalog(resp *agentpbv2.ListModelCatalogResponse) string {
	var b strings.Builder
	if len(resp.GetModels()) == 0 {
		b.WriteString("No models in this agent's catalog yet.\n")
	} else {
		rows := make([][]string, 0, len(resp.GetModels()))
		for _, m := range resp.GetModels() {
			engine, firstStart := "—", m.GetUnavailableReason()
			if v := m.GetVariant(); v != nil {
				engine, firstStart = v.GetEngine(), modelFirstStartCost(v)
			}
			rows = append(rows, []string{m.GetId(), engine, fmt.Sprintf("%d classes", len(m.GetLabels())), firstStart})
		}
		b.WriteString(tui.RenderTable([]string{"Model", "Engine", "Detects", "First start"}, rows))
	}
	if len(resp.GetCameras()) == 0 {
		b.WriteString("No cameras a model can watch.\n")
	} else {
		rows := make([][]string, 0, len(resp.GetCameras()))
		for _, c := range resp.GetCameras() {
			rows = append(rows, []string{c.GetSourceId(), c.GetName()})
		}
		b.WriteString(tui.RenderTable([]string{"Camera", "Name"}, rows))
	}
	fmt.Fprintf(&b, "%d of %d model slots in use\n", resp.GetRunning(), resp.GetMaxRunning())
	return b.String()
}

// modelFirstStartCost says what a first start still has to do on this device.
func modelFirstStartCost(v *agentpbv2.CatalogVariant) string {
	var parts []string
	if !v.GetImageCached() {
		parts = append(parts, "downloads its runtime image")
	}
	if n := v.GetDownloadBytes(); n > 0 {
		parts = append(parts, "downloads "+modelBytes(n)+" of model")
	}
	if v.GetNeedsEngineBuild() {
		parts = append(parts, "builds a TensorRT engine (minutes)")
	}
	if len(parts) == 0 {
		return "ready to start"
	}
	return strings.Join(parts, ", ")
}

func modelBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", (n+1023)/1024)
	}
}

func newDeviceModelListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the models running on a device",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withModelClient(cmd.Context(), func(client agentpbv2.WendyModelServiceClient) error {
				resp, err := client.ListModels(cmd.Context(), &agentpbv2.ListModelsRequest{})
				if err != nil {
					return modelServiceErr(err)
				}
				if jsonOutput {
					return encodeProtoJSON(cmd.OutOrStdout(), resp)
				}
				if len(resp.GetInstances()) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "No models running.")
					return nil
				}
				rows := make([][]string, 0, len(resp.GetInstances()))
				for _, i := range resp.GetInstances() {
					rows = append(rows, []string{i.GetInstanceId(), i.GetModelId(), i.GetCameraSourceId(),
						modelStateText(i), fmt.Sprintf("%d", i.GetWatchers()), shortModelDigest(i.GetFileSha256())})
				}
				fmt.Fprint(cmd.OutOrStdout(), tui.RenderTable([]string{"Instance", "Model", "Camera", "State", "Watchers", "File"}, rows))
				return nil
			})
		},
	}
}

func newDeviceModelStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <instance>",
		Short: "Stop a running model, whoever is watching it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withModelClient(cmd.Context(), func(client agentpbv2.WendyModelServiceClient) error {
				ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
				defer cancel()
				resp, err := client.StopModel(ctx, &agentpbv2.StopModelRequest{InstanceId: args[0]})
				if err != nil {
					return modelServiceErr(err)
				}
				if jsonOutput {
					return encodeProtoJSON(cmd.OutOrStdout(), resp)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Stopped %s (%s).\n", args[0], modelStateLabel(resp.GetInstance()))
				return nil
			})
		},
	}
}

// modelStateLabel is the state and what it is doing, e.g.
// "preparing: pulling host image".
func modelStateLabel(i *agentpbv2.ModelInstance) string {
	state := strings.ToLower(strings.TrimPrefix(i.GetState().String(), "MODEL_STATE_"))
	if d := i.GetStateDetail(); d != "" {
		return state + ": " + d
	}
	return state
}

// modelStateText adds the frame rate to a ready instance's label.
func modelStateText(i *agentpbv2.ModelInstance) string {
	if i.GetState() == agentpbv2.ModelState_MODEL_STATE_READY && i.GetStats().GetProcessedFps() > 0 {
		return fmt.Sprintf("ready (%.1f fps)", i.GetStats().GetProcessedFps())
	}
	return modelStateLabel(i)
}

func shortModelDigest(sha string) string {
	if len(sha) > 12 {
		return sha[:12] + "…"
	}
	return sha
}
```

In `go/internal/cli/commands/device.go`, add `newDeviceModelCmd(),` to the `addToGroup("apps", …)` call, after `newVolumesCmd(),`.

- [ ] **Step 4: Run the tests**

Run: `go test ./go/internal/cli/commands/ -run 'TestDeviceModel|TestNewDeviceCmd'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go/internal/cli/commands/device_model.go go/internal/cli/commands/device_model_test.go go/internal/cli/commands/device.go go/internal/cli/commands/commands_test.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
cli: add wendy device model catalog, list and stop

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 16: CLI: `wendy device model run`

**Files:**
- Modify: `go/internal/cli/commands/device_model.go`: add `run` and register it in `newDeviceModelCmd`
- Modify: `go/internal/cli/commands/device_model_test.go`: add the fake's `StartModel` and `WatchModel`, plus the run tests

**Interfaces:**
- Consumes: Task 15.
- Produces: `newDeviceModelRunCmd()`, `runModelWatch(ctx, client, out io.Writer, modelRunOptions) error` and `modelWatchLine(*agentpbv2.ModelWatchMessage) string`.

- [ ] **Step 1: Write the failing tests**

Append to `go/internal/cli/commands/device_model_test.go`:

```go
func (f *fakeModelService) StartModel(_ context.Context, req *agentpbv2.StartModelRequest) (*agentpbv2.StartModelResponse, error) {
	f.mu.Lock()
	f.start = req
	f.mu.Unlock()
	return &agentpbv2.StartModelResponse{Instance: &agentpbv2.ModelInstance{
		InstanceId: "m-1", State: agentpbv2.ModelState_MODEL_STATE_PREPARING}}, nil
}

func (f *fakeModelService) WatchModel(req *agentpbv2.WatchModelRequest, stream grpc.ServerStreamingServer[agentpbv2.ModelWatchMessage]) error {
	f.mu.Lock()
	f.watch = req
	f.mu.Unlock()
	started := &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Started{Started: &agentpbv2.WatchStarted{
		WatchId: "w-1", Instance: &agentpbv2.ModelInstance{InstanceId: "m-1", State: agentpbv2.ModelState_MODEL_STATE_READY}}}}
	if err := stream.Send(started); err != nil {
		return err
	}
	for _, m := range f.events {
		if err := stream.Send(m); err != nil {
			return err
		}
	}
	if f.hold {
		<-stream.Context().Done()
	}
	return nil
}

func (f *fakeModelService) watched() *agentpbv2.WatchModelRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.watch
}

func personEntered() *agentpbv2.ModelWatchMessage {
	return &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Event{Event: &agentpbv2.ModelEvent{
		Sequence: 1, Type: "entered", ClassName: "person", Confidence: 0.91, TrackId: 12, TimeUnixNanos: time.Now().UnixNano()}}}
}

func finalStatus(state agentpbv2.ModelState, detail string) *agentpbv2.ModelWatchMessage {
	return &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Status{Status: &agentpbv2.ModelInstance{
		InstanceId: "m-1", State: state, StateDetail: detail}}}
}

func TestModelRunPrintsEventsAndDetaches(t *testing.T) {
	fake := &fakeModelService{events: []*agentpbv2.ModelWatchMessage{personEntered(), finalStatus(agentpbv2.ModelState_MODEL_STATE_STOPPED, "stopped")}}
	serveModels(t, fake)
	jsonOutput = false
	var out strings.Builder
	err := withModelClient(context.Background(), func(c agentpbv2.WendyModelServiceClient) error {
		return runModelWatch(context.Background(), c, &out, modelRunOptions{Model: "coco-detector", Camera: "v4l2:/dev/video0", Classes: []string{"person"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "person") || !strings.Contains(out.String(), "entered") || !strings.Contains(out.String(), "stopped") {
		t.Fatalf("output = %q", out.String())
	}
	if got := fake.watched().GetClasses(); len(got) != 1 || got[0] != "person" {
		t.Fatalf("watched classes = %v", got)
	}
	if got := fake.stops(); len(got) != 1 || got[0] != "m-1/w-1" {
		t.Fatalf("stops = %v, want the watch detached", got)
	}
}

func TestModelRunDetachesOnInterrupt(t *testing.T) {
	fake := &fakeModelService{events: []*agentpbv2.ModelWatchMessage{personEntered()}, hold: true}
	serveModels(t, fake)
	jsonOutput = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var out strings.Builder
	go func() {
		done <- withModelClient(ctx, func(c agentpbv2.WendyModelServiceClient) error {
			return runModelWatch(ctx, c, &out, modelRunOptions{Model: "coco-detector", Camera: "v4l2:/dev/video0"})
		})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for fake.watched() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let the started message arrive
	cancel()                          // Ctrl+C
	if err := <-done; err != nil {
		t.Fatalf("an interrupted run returned %v", err)
	}
	if got := fake.stops(); len(got) != 1 || got[0] != "m-1/w-1" {
		t.Fatalf("stops = %v, want the watch detached after Ctrl+C", got)
	}
}

func TestModelRunReportsFailure(t *testing.T) {
	serveModels(t, &fakeModelService{events: []*agentpbv2.ModelWatchMessage{finalStatus(agentpbv2.ModelState_MODEL_STATE_FAILED, "no frames from camera")}})
	jsonOutput = false
	err := withModelClient(context.Background(), func(c agentpbv2.WendyModelServiceClient) error {
		return runModelWatch(context.Background(), c, &strings.Builder{}, modelRunOptions{Model: "coco-detector", Camera: "v4l2:/dev/video0"})
	})
	if err == nil || !strings.Contains(err.Error(), "no frames from camera") {
		t.Fatalf("err = %v", err)
	}
}
```

Add `"time"` to the test file's imports.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./go/internal/cli/commands/ -run TestModelRun`
Expected: FAIL to compile with `undefined: runModelWatch`.

- [ ] **Step 3: Implement `run`**

In `go/internal/cli/commands/device_model.go`:

1. Add `"io"` to the imports.
2. In `newDeviceModelCmd`, change the `AddCommand` line to:

```go
	cmd.AddCommand(newDeviceModelCatalogCmd(), newDeviceModelRunCmd(), newDeviceModelListCmd(), newDeviceModelStopCmd())
```

3. Append:

```go
func newDeviceModelRunCmd() *cobra.Command {
	var opts modelRunOptions
	cmd := &cobra.Command{
		Use:   "run <model>",
		Short: "Start a model on a camera and print what it sees until Ctrl+C",
		Example: `  wendy device model run coco-detector --camera v4l2:/dev/video0 --watch person
  wendy device model run coco-detector --camera v4l2:/dev/video0 --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.Camera == "" {
				return errors.New("choose a camera with --camera; `wendy device model catalog` lists them")
			}
			opts.Model = args[0]
			return withModelClient(cmd.Context(), func(client agentpbv2.WendyModelServiceClient) error {
				return runModelWatch(cmd.Context(), client, cmd.OutOrStdout(), opts)
			})
		},
	}
	cmd.Flags().StringVar(&opts.Camera, "camera", "", "Camera source to watch, e.g. v4l2:/dev/video0")
	cmd.Flags().StringSliceVar(&opts.Classes, "watch", nil, "Only report these classes (default: all)")
	cmd.Flags().Float32Var(&opts.MinConfidence, "min-confidence", 0, "Only report detections at least this confident (0-1)")
	cmd.Flags().StringVar(&opts.Label, "label", "", "A name for this watch, shown to other clients")
	return cmd
}

type modelRunOptions struct {
	Model, Camera, Label string
	Classes              []string
	MinConfidence        float32
}

// runModelWatch starts the model, watches it until ctx ends or the model
// stops, then detaches, so the device stops the model unless someone else is
// watching.
func runModelWatch(ctx context.Context, client agentpbv2.WendyModelServiceClient, out io.Writer, opts modelRunOptions) error {
	started, err := client.StartModel(ctx, &agentpbv2.StartModelRequest{ModelId: opts.Model, CameraSourceId: opts.Camera})
	if err != nil {
		return modelServiceErr(err)
	}
	id := started.GetInstance().GetInstanceId()
	stream, err := client.WatchModel(ctx, &agentpbv2.WatchModelRequest{
		InstanceId: id, Label: opts.Label, Classes: opts.Classes, MinConfidence: opts.MinConfidence})
	if err != nil {
		return modelServiceErr(err)
	}
	var watchID, lastStatusLine string
	var last *agentpbv2.ModelInstance
	defer func() {
		if watchID == "" {
			return
		}
		// Detach on the way out, even after Ctrl+C has cancelled ctx.
		detachCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = client.StopModel(detachCtx, &agentpbv2.StopModelRequest{InstanceId: id, WatchId: watchID})
	}()
	if !jsonOutput {
		cliLogln("Watching %s on %s (instance %s). Press Ctrl+C to stop.", opts.Model, opts.Camera, id)
	}
	for {
		msg, err := stream.Recv()
		if ctx.Err() != nil {
			return nil
		}
		if err == io.EOF {
			if last.GetState() == agentpbv2.ModelState_MODEL_STATE_FAILED {
				return fmt.Errorf("the model failed: %s", last.GetStateDetail())
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("watching %s: %w", id, modelServiceErr(err))
		}
		switch {
		case msg.GetStarted() != nil:
			watchID, last = msg.GetStarted().GetWatchId(), msg.GetStarted().GetInstance()
		case msg.GetStatus() != nil:
			last = msg.GetStatus()
		}
		if jsonOutput {
			if err := encodeProtoJSON(out, msg); err != nil {
				return err
			}
			continue
		}
		line := modelWatchLine(msg)
		if msg.GetStarted() != nil || msg.GetStatus() != nil {
			if line == lastStatusLine {
				continue // heartbeats repeat the state
			}
			lastStatusLine = line
		}
		if line != "" {
			fmt.Fprintln(out, line)
		}
	}
}

// modelWatchLine renders one watch message for a person.
func modelWatchLine(msg *agentpbv2.ModelWatchMessage) string {
	switch {
	case msg.GetStarted() != nil:
		return "state: " + modelStateLabel(msg.GetStarted().GetInstance())
	case msg.GetStatus() != nil:
		return "state: " + modelStateLabel(msg.GetStatus())
	case msg.GetEvent() != nil:
		e := msg.GetEvent()
		at := time.Unix(0, e.GetTimeUnixNanos()).Format("15:04:05")
		return fmt.Sprintf("%s  %-12s %.2f %s (track %d)", at, e.GetClassName(), e.GetConfidence(), e.GetType(), e.GetTrackId())
	case msg.GetGap() != nil:
		return fmt.Sprintf("missed events %d-%d", msg.GetGap().GetFirstMissing(), msg.GetGap().GetLastMissing())
	}
	return ""
}
```

- [ ] **Step 4: Run the tests and the CLI's command tests**

Run: `go test -race ./go/internal/cli/commands/ -run 'TestModelRun|TestDeviceModel|TestNewDeviceCmd'`
Expected: PASS.
Run: `go run ./go/cmd/wendy device model run --help`
Expected: help text with `--camera`, `--watch`, `--min-confidence` and `--label`.

- [ ] **Step 5: Commit**

```bash
git add go/internal/cli/commands/device_model.go go/internal/cli/commands/device_model_test.go
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
cli: add wendy device model run

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

---

### Task 17: The fake model host, its workflow, and a device smoke test

**Files:**
- Create: `go/modelhost/fakehost/main.go`
- Test: `go/modelhost/fakehost/main_test.go`
- Create: `go/modelhost/fakehost/Dockerfile`
- Create: `go/modelhost/fakehost/README.md`
- Create: `.github/workflows/model-host.yml`

**Interfaces:**
- Consumes: the host contract. That is the data-socket framing (a 4-byte big-endian length followed by JSON), the acks `{"version","state","error"}`, the record names from Task 4, and `WENDY_DATA_SOCKET`, `WENDY_MODEL_VARIANT` and `WENDY_CAMERA_SOURCE` from Task 6.
- Produces: the image `ghcr.io/wendylabsinc/wendy-model-host-fake`. It reports ready, heartbeats every 5 s, and alternates a person entering and leaving every `FAKEHOST_PERIOD_SECONDS` (default 20).

- [ ] **Step 1: Write the failing test**

`go/modelhost/fakehost/main_test.go`:

```go
package main

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestHostSpeaksTheDataSocketContract(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	h := &host{conn: client, variant: "fakehost", source: "v4l2:/dev/video0"}
	got := make(chan record, 4)
	go func() { // the agent's side: read a frame, acknowledge it
		for {
			var prefix [4]byte
			if _, err := io.ReadFull(server, prefix[:]); err != nil {
				return
			}
			body := make([]byte, binary.BigEndian.Uint32(prefix[:]))
			if _, err := io.ReadFull(server, body); err != nil {
				return
			}
			var r record
			_ = json.Unmarshal(body, &r)
			got <- r
			ack, _ := json.Marshal(map[string]any{"version": 1, "state": "buffered"})
			binary.BigEndian.PutUint32(prefix[:], uint32(len(ack)))
			_, _ = server.Write(append(prefix[:], ack...))
		}
	}()
	stop := make(chan os.Signal)
	heartbeat := make(chan time.Time)
	period := make(chan time.Time)
	done := make(chan error, 1)
	go func() { done <- h.loop(stop, heartbeat, period) }()

	if r := <-got; r.Name != "model.status" || r.Attributes["state"] != "ready" {
		t.Fatalf("first record = %+v", r)
	}
	period <- time.Now()
	if r := <-got; r.Name != "model.entered" || r.Attributes["class"] != "person" || r.Inputs[0].SampleID != 1 || r.Inputs[0].SourceID != "v4l2:/dev/video0" {
		t.Fatalf("second record = %+v", r)
	}
	heartbeat <- time.Now()
	if r := <-got; r.Name != "model.status" {
		t.Fatalf("heartbeat record = %+v", r)
	}
	period <- time.Now()
	if r := <-got; r.Name != "model.left" {
		t.Fatalf("fourth record = %+v", r)
	}
	stop <- syscall.SIGTERM
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./go/modelhost/fakehost/`
Expected: FAIL to compile with `undefined: host`.

- [ ] **Step 3: Write the fake host**

`go/modelhost/fakehost/main.go`:

```go
// Command fakehost is a stand-in model host for testing the agent's model
// service before real hosts exist. It speaks the host side of the contract in
// internal/agent/models: it reports ready, heartbeats, and reports a person
// entering and leaving on a fixed period. It never reads the camera node it
// is given.
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

type record struct {
	Version    int            `json:"version"`
	Type       string         `json:"type"`
	Name       string         `json:"name"`
	Model      string         `json:"model,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Inputs     []sampleRef    `json:"inputs,omitempty"`
	BootID     string         `json:"boot_id"`
}

type sampleRef struct {
	SourceID string `json:"source_id"`
	SampleID uint64 `json:"sample_id"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fakehost:", err)
		os.Exit(1)
	}
}

func run() error {
	sock := os.Getenv("WENDY_DATA_SOCKET")
	if sock == "" {
		return errors.New("WENDY_DATA_SOCKET is not set")
	}
	period := 20 * time.Second
	if v := os.Getenv("FAKEHOST_PERIOD_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("FAKEHOST_PERIOD_SECONDS=%q is not a positive integer", v)
		}
		period = time.Duration(n) * time.Second
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return err
	}
	defer conn.Close()
	h := &host{conn: conn, variant: os.Getenv("WENDY_MODEL_VARIANT"), source: os.Getenv("WENDY_CAMERA_SOURCE")}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	return h.loop(stop, time.NewTicker(5*time.Second).C, time.NewTicker(period).C)
}

type host struct {
	conn    net.Conn
	variant string
	source  string
	sample  uint64
	inView  bool
}

// loop reports ready, heartbeats on every tick, and alternates a person
// entering and leaving on every period.
func (h *host) loop(stop <-chan os.Signal, heartbeat, period <-chan time.Time) error {
	if err := h.status("ready"); err != nil {
		return err
	}
	for {
		select {
		case <-stop:
			return nil
		case <-heartbeat:
			if err := h.status("ready"); err != nil {
				return err
			}
		case <-period:
			h.inView = !h.inView
			name := "model.left"
			if h.inView {
				name = "model.entered"
			}
			if err := h.detection(name); err != nil {
				return err
			}
		}
	}
}

func (h *host) status(state string) error {
	return h.send(record{Version: 1, Type: "event", Name: "model.status", BootID: "unavailable",
		Attributes: map[string]any{"state": state, "processed_fps": 10.0, "latency_p50_ms": 1.0}})
}

func (h *host) detection(name string) error {
	h.sample++
	return h.send(record{Version: 1, Type: "event", Name: name, Model: h.variant, BootID: "unavailable",
		Attributes: map[string]any{"class": "person", "confidence": 0.9, "track_id": 1,
			"box": map[string]any{"x": 0.4, "y": 0.2, "width": 0.2, "height": 0.6}},
		Inputs: []sampleRef{{SourceID: h.source, SampleID: h.sample}}})
}

// send writes one length-prefixed record and reads the agent's
// acknowledgement, framed as internal/agent/services/app_data_socket.go does.
func (h *host) send(r record) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(body)))
	if _, err := h.conn.Write(append(prefix[:], body...)); err != nil {
		return err
	}
	if _, err := io.ReadFull(h.conn, prefix[:]); err != nil {
		return err
	}
	ack := make([]byte, binary.BigEndian.Uint32(prefix[:]))
	if _, err := io.ReadFull(h.conn, ack); err != nil {
		return err
	}
	var parsed struct {
		State string `json:"state"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(ack, &parsed); err != nil {
		return err
	}
	if parsed.State == "rejected" {
		return fmt.Errorf("the agent rejected %s: %s", r.Name, parsed.Error)
	}
	return nil
}
```

`go/modelhost/fakehost/Dockerfile`, built from the repo root with BuildKit (`docker buildx`). The build stage runs on the build machine and cross-compiles, so an arm64 image builds on an x86 host without QEMU:

```dockerfile
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY go/modelhost/fakehost ./go/modelhost/fakehost
RUN go test ./go/modelhost/fakehost/... && CGO_ENABLED=0 GOARCH=$TARGETARCH go build -trimpath -o /out/fakehost ./go/modelhost/fakehost

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/fakehost /usr/local/bin/fakehost
ENTRYPOINT ["/usr/local/bin/fakehost"]
```

`.github/workflows/model-host.yml`:

```yaml
name: Model hosts

on:
  pull_request:
    paths:
      - 'go/modelhost/**'
      - '.github/workflows/model-host.yml'
  workflow_dispatch:
    inputs:
      publish:
        description: Publish the fake model host for development catalogs
        type: boolean
        default: false

permissions:
  contents: read

jobs:
  fakehost:
    runs-on: ubuntu-24.04-arm
    timeout-minutes: 20
    permissions:
      contents: read
      packages: write
    steps:
      - uses: actions/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0
      - name: Test and build
        run: docker build -f go/modelhost/fakehost/Dockerfile -t fakehost:test .
      - name: Publish
        if: github.event_name == 'workflow_dispatch' && inputs.publish
        env:
          REGISTRY_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          IMAGE: ghcr.io/wendylabsinc/wendy-model-host-fake:dev
        run: |
          printf '%s' "$REGISTRY_TOKEN" | docker login ghcr.io -u "$GITHUB_ACTOR" --password-stdin
          docker tag fakehost:test "$IMAGE"
          docker push "$IMAGE"
          docker buildx imagetools inspect "$IMAGE" --format '{{json .Manifest.Digest}}'
```

`go/modelhost/fakehost/README.md`:

````markdown
# fakehost

A stand-in model host for the agent's model service
(`specs/2026-09-25-model-watch-design.md`). It speaks the host side of the
contract in `go/internal/agent/models`:

- It reports `ready`, then sends a `model.status` heartbeat every 5 s.
- It reports a `person` entering, then leaving, every `FAKEHOST_PERIOD_SECONDS`
  (default 20).
- It never reads the camera node it is given, so it tests the agent's model
  service, not perception.

Once published, reference it from a development catalog by digest, and point
the agent at that catalog with `WENDY_MODEL_CATALOG_FILE`. The smoke test in
`specs/2026-09-25-model-watch-plan-2-agent-service-part-2.md` (Task 17) walks through it.
````

- [ ] **Step 4: Run the test and build the image locally**

Run: `go test ./go/modelhost/fakehost/`
Expected: PASS.
Prerequisite: Docker with the buildx plugin, which the Dockerfile's `$BUILDPLATFORM` needs. The development machine this plan was written on lacked it; on Arch or CachyOS, install it with `sudo pacman -S docker-buildx`.
Run: `docker buildx build --load -f go/modelhost/fakehost/Dockerfile -t fakehost:test .`
Expected: the build succeeds, and the `go test` line inside it passes.

- [ ] **Step 5: Commit**

```bash
git add go/modelhost/fakehost .github/workflows/model-host.yml
git -c user.name=Ethan -c user.email=ebrogames@gmail.com commit -F- <<'EOF'
modelhost: add a fake model host for testing the model service

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019FBeZXTxFQqYE4f8GkNnND
EOF
```

- [ ] **Step 6: Smoke-test on a device**

This step is manual and needs a person. Use a Jetson with a USB camera, since the two-plane path is proven there (`Examples/WendyDataModelApp`). Record the outcome in the PR description.

1. **Publish the fake host for this branch.** The workflow can only be dispatched once it is on the default branch.
   ```bash
   echo "$GHCR_TOKEN" | docker login ghcr.io -u "$GITHUB_USER" --password-stdin   # a token with write:packages
   docker buildx build --platform linux/arm64,linux/amd64 -f go/modelhost/fakehost/Dockerfile \
     -t ghcr.io/wendylabsinc/wendy-model-host-fake:m2-smoke --push .
   DIGEST=$(docker buildx imagetools inspect ghcr.io/wendylabsinc/wendy-model-host-fake:m2-smoke --format '{{json .Manifest.Digest}}' | tr -d '"')
   echo "$DIGEST"
   ```
   If the package is new, make it public once, as the ROS 2 inspector image is: GitHub → wendylabsinc → Packages → wendy-model-host-fake → Package settings → Change visibility → Public.

2. **Write the development catalog.** The model file is this repo's `LICENSE` at a pinned commit; the fake host never reads it.
   ```bash
   cat > /tmp/model-catalog.dev.json <<EOF
   {
     "version": "dev-fakehost",
     "models": [{
       "id": "fake-detector",
       "description": "Reports a person entering and leaving every 20 s; reads no frames",
       "kind": "detector",
       "labels": ["person"],
       "variants": [{
         "id": "fakehost",
         "engine": "onnxruntime",
         "requires": {},
         "host_image": "ghcr.io/wendylabsinc/wendy-model-host-fake@${DIGEST}",
         "file": {
           "url": "https://raw.githubusercontent.com/wendylabsinc/WendyOS/f36f361afec38d064f383203fb75b85f9411a2b0/LICENSE",
           "sha256": "c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4",
           "bytes": 11357
         },
         "input_size": 1
       }]
     }]
   }
   EOF
   ```

3. **Install this branch's agent with the catalog.**
   ```bash
   (cd go && make build-agent-linux-arm64)
   wendy device shell --device <jetson>
   ```
   In that shell:
   1. Create `/var/lib/wendy/models/catalog.dev.json`: run `cat > /var/lib/wendy/models/catalog.dev.json`, paste the file from step 2, then press Ctrl+D.
   2. Add a systemd drop-in that sets `Environment=WENDY_MODEL_CATALOG_FILE=/var/lib/wendy/models/catalog.dev.json`. The agent unit is `edge-agent.service` on WendyOS images and `wendy-agent.service` from the Debian and Arch packages. Use `systemctl edit <unit>`.
   3. Exit the shell.

   Then run:
   ```bash
   wendy device update --binary go/bin/wendy-agent-linux-arm64 --device <jetson>
   ```

4. **Exercise the service**, using the CLI built from this branch:
   ```bash
   go run ./go/cmd/wendy device model catalog --device <jetson>
   go run ./go/cmd/wendy device model run fake-detector --camera v4l2:/dev/video0 --device <jetson>
   ```
   Expected:
   - `catalog` lists `fake-detector` and the camera.
   - `run` prints `state: preparing: pulling host image`, then `state: ready`, then a `person … entered` line about 20 s later and `left` about 20 s after that.
   - In a second terminal, `wendy device model list --device <jetson>` shows the instance as `ready (10.0 fps)` with 1 watcher.

5. **Check cleanup.** Kill the `run` process with `kill -9` so it cannot detach, then time how long the instance takes to go. `wendy device model list` must show nothing within 90 s. After that, `wendy device shell` followed by `ctr -n default containers ls | grep wendy-model-` must print nothing.

6. **Check the boot cleanup.** Start `run` again, and restart the agent with `systemctl restart <unit>` in a device shell. The `run` stream ends. Within a few seconds of the agent coming back, `ctr -n default containers ls | grep wendy-model-` prints nothing.

7. **Remove the drop-in** and restart the agent, so the device returns to the built-in (empty) catalog.

---

## Self-Review Notes

- **Spec coverage.** Each part of the spec that M2 owns has a task:

  | Spec section | Task |
  |---|---|
  | §5 contract | 2 |
  | §6.2 environment | 6: `HostSpec.Env` |
  | §6.4 catalog rules | 3 |
  | §7.2 steps 1–7 | 6, 9, 12 |
  | §7.3 leases, restarts, cleanup | 7, 9 |
  | §7.4 demand | 10 |
  | §7.5 tap | 11, 8 |
  | §8.1 CLI | 15, 16 |
  | §9 rows owned by the agent | 6–9, 12, 13 |
  | §10 container lockdown | 12 |
  | §10 no free-form artifacts | 3, 14 |
  | §11 agent tests | throughout |
- **Deferred elsewhere.** The model host itself (§6.1) is milestone M1; the TensorRT and QNN images are M4; the MCP tools and chat (§8.2, §8.3) are M3.
- **Placeholders.** None. The image digest in Task 17 comes from the command that prints it. `<jetson>` and `<unit>` in the manual smoke test name the tester's own device and its agent unit.
- **Name consistency.** Names are checked across tasks:
  - `Supervisor.Stop(ctx, instanceID, watchID)` keeps one signature from Task 6 on.
  - `Watch` returns `(*Watch, InstanceInfo, uint64, error)` in Tasks 7, 8 and 13.
  - `modelStateLabel` and `modelStateText` are defined in Task 15 and used in Task 16.
  - `resolveModelCamera` and `modelHostVideoGID` are defined in Task 12 and used in its test.
