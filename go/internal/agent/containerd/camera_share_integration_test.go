//go:build linux

package containerd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Camera sharing's container lifecycle against a real containerd and runc,
// with a fake share manager that records its calls. Run under the private
// daemon of .github/scripts/test-containerd-regressions.sh.

// shareAppIdle keeps an app running until it is stopped, and exits at once
// on SIGTERM, which a bare sleep as pid 1 would ignore for the stop's grace.
const shareAppIdle = `trap 'exit 0' TERM; while :; do sleep 1; done`

var (
	sharedCamera = []appconfig.Entitlement{{Type: appconfig.EntitlementCamera}}
	directCamera = []appconfig.Entitlement{{Type: appconfig.EntitlementCamera, Mode: appconfig.CameraModeDirect}}
)

// newCameraShareClient returns an integration client with the fake share
// manager wired in and the busybox image ready. Its logger outlives the test:
// the lifecycle's background syncs may still log after it ends.
func newCameraShareClient(t *testing.T) (*Client, context.Context, *fakeCameraShares, *observer.ObservedLogs) {
	t.Helper()
	c, ctx := newIntegrationClient(t)
	core, logs := observer.New(zapcore.DebugLevel)
	c.logger = zap.New(core)
	fake := &fakeCameraShares{}
	c.SetCameraShareProvider(fake)
	createReplaceTestImage(t, c, ctx)
	return c, ctx, fake, logs
}

// createShareApp creates appID's container through the production create path
// with the given entitlements, running script under /bin/sh, and deletes it
// when the test ends.
func createShareApp(t *testing.T, c *Client, ctx context.Context, appID, version string, entitlements []appconfig.Entitlement, script string) {
	t.Helper()
	req := &agentpb.CreateContainerRequest{AppName: appID, ImageName: replaceTestImage, Cmd: "/bin/sh", UserArgs: []string{"-c", script}}
	cfg := &appconfig.AppConfig{AppID: appID, Version: version, Entitlements: entitlements}
	if err := c.CreateContainerWithProgress(ctx, req, cfg, nil); err != nil {
		t.Fatalf("creating %s: %v", appID, err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if err := c.DeleteContainer(cleanup, appID, false); err != nil && !strings.Contains(err.Error(), "not found") {
			t.Errorf("cleanup %s: %v", appID, err)
		}
	})
}

// startShareApp starts appID and returns its output stream's stdout lines,
// read in the background, and a channel closed when the stream ends.
func startShareApp(t *testing.T, c *Client, ctx context.Context, appID string) (<-chan string, <-chan struct{}) {
	t.Helper()
	stream, err := c.StartContainer(ctx, appID, "", nil)
	if err != nil {
		t.Fatalf("starting %s: %v", appID, err)
	}
	lines := make(chan string, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var pending []byte
		for out := range stream {
			pending = append(pending, out.Stdout...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				select {
				case lines <- string(pending[:i]):
				default:
				}
				pending = pending[i+1:]
			}
			if out.Done && out.PublishIfCurrent != nil {
				out.PublishIfCurrent(func() {})
			}
		}
	}()
	return lines, done
}

func firstLine(t *testing.T, lines <-chan string) string {
	t.Helper()
	select {
	case line := <-lines:
		return line
	case <-time.After(20 * time.Second):
		t.Fatal("the app printed nothing")
		return ""
	}
}

func taskPID(t *testing.T, c *Client, ctx context.Context, appID string) uint32 {
	t.Helper()
	ctr, err := c.client.LoadContainer(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := ctr.Task(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return task.Pid()
}

func shareLabel(t *testing.T, c *Client, ctx context.Context, appID string) (string, bool) {
	t.Helper()
	ctr, err := c.client.LoadContainer(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	labels, err := ctr.Labels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := labels[labelKeyCameraShare]
	return v, ok
}

// shareTestImage is the busybox image newCameraShareClient made.
func shareTestImage(t *testing.T, c *Client, ctx context.Context) containerd.Image {
	t.Helper()
	img, err := c.client.GetImage(ctx, normalizeImageName(replaceTestImage))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// eventually polls cond for up to 10 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The cover runs between NewTask and Start: the task's process is still the
// runtime's init, not the entrypoint, and the entrypoint's first line already
// sees what the cover did.
func TestIntegrationCameraShareCoverRunsBeforeTheEntrypoint(t *testing.T) {
	c, ctx, fake, _ := newCameraShareClient(t)
	const app = "share-cover"
	var exeAtCover string
	fake.setCover(func(_ string, pid uint32) []error {
		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if err != nil {
			exe = "unreadable: " + err.Error()
		}
		exeAtCover = exe
		// An entrypoint already running would print before this returns.
		time.Sleep(150 * time.Millisecond)
		if err := os.WriteFile(fmt.Sprintf("/proc/%d/root/covered", pid), nil, 0o644); err != nil {
			return []error{err}
		}
		return nil
	})
	createShareApp(t, c, ctx, app, "1", sharedCamera,
		`if [ -e /covered ]; then echo after-cover; else echo before-cover; fi; `+shareAppIdle)
	if v, _ := shareLabel(t, c, ctx, app); v != "shared" {
		t.Fatalf("share label = %q, want shared", v)
	}

	lines, done := startShareApp(t, c, ctx, app)
	if got := firstLine(t, lines); got != "after-cover" {
		t.Fatalf("first line = %q; the entrypoint ran before the cover returned", got)
	}
	if exeAtCover == "" || exeAtCover == "/bin/busybox" || strings.HasPrefix(exeAtCover, "unreadable") {
		t.Fatalf("/proc/<pid>/exe at the cover = %q; want the runtime's init, not the entrypoint", exeAtCover)
	}
	pid := taskPID(t, c, ctx, app)
	// The same check after Start sees the entrypoint, so it tells the two apart.
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err != nil || exe != "/bin/busybox" {
		t.Fatalf("/proc/<pid>/exe after start = %q, %v; want /bin/busybox", exe, err)
	}
	if got := fake.callsFor(app); len(got) < 2 || got[0] != (shareCall{op: "create", containerID: app, appID: app}) || got[1] != (shareCall{op: "cover", containerID: app, pid: pid}) {
		t.Fatalf("calls = %+v; want create, then a cover of the task's pid %d", got, pid)
	}

	if err := c.StopContainer(ctx, app); err != nil {
		t.Fatal(err)
	}
	<-done
}

// Cover errors never stop or delay a start: the task runs, its output arrives,
// and each error is logged with the app's name.
func TestIntegrationCameraShareCoverFailureStillStarts(t *testing.T) {
	c, ctx, fake, logs := newCameraShareClient(t)
	const app = "share-cover-fails"
	fake.setCover(func(string, uint32) []error {
		return []error{errors.New("injected: setns failed"), errors.New("injected: cover timed out")}
	})
	createShareApp(t, c, ctx, app, "1", sharedCamera, `echo running; `+shareAppIdle)

	lines, done := startShareApp(t, c, ctx, app)
	if got := firstLine(t, lines); got != "running" {
		t.Fatalf("first line = %q, want running", got)
	}
	ctr, err := c.client.LoadContainer(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	task, err := ctr.Task(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := task.Status(ctx); err != nil || st.Status != containerd.Running {
		t.Fatalf("task status = %+v, %v; want running", st, err)
	}
	logged := logs.FilterField(zap.String("app_name", app)).FilterMessageSnippet("camera").All()
	var errs []string
	for _, e := range logged {
		if s, ok := e.ContextMap()["error"].(string); ok {
			errs = append(errs, s)
		}
	}
	if !slices.Contains(errs, "injected: setns failed") || !slices.Contains(errs, "injected: cover timed out") {
		t.Fatalf("cover errors logged with app_name = %q; want both", errs)
	}
	if slices.Contains(fake.ops(app), "release") {
		t.Fatalf("a started app's shares were released: %v", fake.ops(app))
	}

	if err := c.StopContainer(ctx, app); err != nil {
		t.Fatal(err)
	}
	<-done
}

// Review Focus 1: a create makes the shares; a redeploy removes the old
// container's before it makes the new one's; a delete removes them. A direct
// app, and a container made before sharing existed, are never passed on.
func TestIntegrationCameraShareNodesFollowCreateReplaceAndDelete(t *testing.T) {
	c, ctx, fake, _ := newCameraShareClient(t)
	lifecycle := []string{"create", "remove"}

	const app = "share-life"
	createShareApp(t, c, ctx, app, "1", sharedCamera, shareAppIdle)
	if got := fake.ops(app, lifecycle...); !reflect.DeepEqual(got, []string{"create"}) {
		t.Fatalf("after create: %v, want [create]", got)
	}
	if got := fake.callsFor(app)[0]; got.appID != app {
		t.Fatalf("create's app id = %q, want %q", got.appID, app)
	}
	// Redeploy over a running task, as `wendy run` does while iterating.
	_, done := startShareApp(t, c, ctx, app)
	createShareApp(t, c, ctx, app, "2", sharedCamera, shareAppIdle)
	if got := fake.ops(app, lifecycle...); !reflect.DeepEqual(got, []string{"create", "remove", "create"}) {
		t.Fatalf("after redeploy: %v, want [create remove create]", got)
	}
	<-done
	if err := c.DeleteContainer(ctx, app, false); err != nil {
		t.Fatal(err)
	}
	if got := fake.ops(app, lifecycle...); !reflect.DeepEqual(got, []string{"create", "remove", "create", "remove"}) {
		t.Fatalf("after delete: %v, want [create remove create remove]", got)
	}

	const direct = "share-direct"
	createShareApp(t, c, ctx, direct, "1", directCamera, shareAppIdle)
	if v, _ := shareLabel(t, c, ctx, direct); v != "direct" {
		t.Fatalf("direct app's share label = %q, want direct", v)
	}
	_, done = startShareApp(t, c, ctx, direct)
	createShareApp(t, c, ctx, direct, "2", directCamera, shareAppIdle)
	<-done
	if err := c.DeleteContainer(ctx, direct, false); err != nil {
		t.Fatal(err)
	}
	if got := fake.callsFor(direct); len(got) != 0 {
		t.Fatalf("a direct app was passed to the share manager: %+v", got)
	}

	const plain = "share-plain"
	createShareApp(t, c, ctx, plain, "1", nil, shareAppIdle)
	if _, ok := shareLabel(t, c, ctx, plain); ok {
		t.Fatal("an app without a camera has a share label")
	}

	// R4: a camera container made before this change has no label. Its
	// redeploy removes nothing of its own; the new container shares.
	const old = "share-pre-existing"
	startOldApp(t, c, ctx, shareTestImage(t, c, ctx), old, sharedCamera)
	if _, ok := shareLabel(t, c, ctx, old); ok {
		t.Fatal("the pre-existing fixture has a share label")
	}
	createShareApp(t, c, ctx, old, "2", sharedCamera, shareAppIdle)
	if got := fake.ops(old, lifecycle...); !reflect.DeepEqual(got, []string{"create"}) {
		t.Fatalf("redeploying a pre-existing container: %v, want only the new container's create", got)
	}
	for _, sync := range fake.allSyncs() {
		for _, sc := range sync {
			if sc.ID == direct || sc.ID == plain {
				t.Fatalf("a sync passed on %s: %+v", sc.ID, sync)
			}
		}
	}
}

// A shared container's namespace is let go of when its task exits on its own,
// when it is stopped, and when a start fails after the cover.
func TestIntegrationCameraShareReleasedWhenTheTaskEnds(t *testing.T) {
	c, ctx, fake, _ := newCameraShareClient(t)

	const exits = "share-exits"
	createShareApp(t, c, ctx, exits, "1", sharedCamera, `echo bye; exit 3`)
	_, done := startShareApp(t, c, ctx, exits)
	<-done
	eventually(t, "the exited task's shares are released", func() bool {
		return reflect.DeepEqual(fake.ops(exits, "cover", "release"), []string{"cover", "release"})
	})

	const stopped = "share-stopped"
	createShareApp(t, c, ctx, stopped, "1", sharedCamera, shareAppIdle)
	_, done = startShareApp(t, c, ctx, stopped)
	if err := c.StopContainer(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	if got := fake.ops(stopped, "cover", "release"); len(got) < 2 || got[0] != "cover" || got[1] != "release" {
		t.Fatalf("after stop: %v, want cover then release", got)
	}
	<-done

	// The start's request is cancelled during the cover: the start fails
	// after it, and the shares it covered are let go of.
	const failed = "share-start-fails"
	createShareApp(t, c, ctx, failed, "1", sharedCamera, shareAppIdle)
	startCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	fake.setCover(func(containerID string, _ uint32) []error {
		if containerID == failed {
			cancel()
		}
		return nil
	})
	if _, err := c.StartContainer(startCtx, failed, "", nil); err == nil {
		t.Fatal("start succeeded after its request was cancelled")
	}
	if got := fake.ops(failed, "cover", "release"); !reflect.DeepEqual(got, []string{"cover", "release"}) {
		t.Fatalf("after a failed start: %v, want cover then release", got)
	}
}

// The periodic sync passes on every "shared" container, running or not, with
// a running one's task pid, and nothing else.
func TestIntegrationCameraShareSyncPassesSharedContainers(t *testing.T) {
	c, ctx, fake, _ := newCameraShareClient(t)
	createShareApp(t, c, ctx, "sync-running", "1", sharedCamera, shareAppIdle)
	createShareApp(t, c, ctx, "sync-idle", "1", sharedCamera, shareAppIdle)
	createShareApp(t, c, ctx, "sync-direct", "1", directCamera, shareAppIdle)
	startOldApp(t, c, ctx, shareTestImage(t, c, ctx), "sync-pre-existing", sharedCamera)
	_, done := startShareApp(t, c, ctx, "sync-running")
	pid := taskPID(t, c, ctx, "sync-running")

	c.SyncCameraLoopbacks(ctx)
	want := []services.CameraShareContainer{{ID: "sync-idle"}, {ID: "sync-running", Running: true, PID: pid}}
	found := false
	for _, sync := range fake.allSyncs() {
		got := append([]services.CameraShareContainer(nil), sync...)
		slices.SortFunc(got, func(a, b services.CameraShareContainer) int { return strings.Compare(a.ID, b.ID) })
		for _, sc := range got {
			if sc.ID == "sync-direct" || sc.ID == "sync-pre-existing" {
				t.Fatalf("a sync passed on %s: %+v", sc.ID, got)
			}
		}
		found = found || reflect.DeepEqual(got, want)
	}
	if !found {
		t.Fatalf("no sync passed %+v; syncs: %+v", want, fake.allSyncs())
	}

	if err := c.StopContainer(ctx, "sync-running"); err != nil {
		t.Fatal(err)
	}
	<-done
}
