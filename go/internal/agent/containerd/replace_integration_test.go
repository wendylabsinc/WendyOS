//go:build linux

package containerd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// WDY-3850: a redeploy that fails must leave the app it was replacing
// running. These tests run the old app's task from a real image, so besides
// root and the containerd socket they need to pull busybox. Like the unpack
// tests, each uses a fresh namespace and leaves its images and snapshots
// behind, so point the variable at a disposable containerd:
//
//	WENDY_CONTAINERD_INTEGRATION=/run/containerd/containerd.sock go test -run Integration ./internal/agent/containerd

const replaceTestImage = "docker.io/library/busybox:1.36"

func pullReplaceTestImage(t *testing.T, c *Client, ctx context.Context) containerd.Image {
	t.Helper()
	img, err := c.client.Pull(ctx, replaceTestImage, containerd.WithPullUnpack, containerd.WithPullSnapshotter(c.snapshotter))
	if err != nil {
		t.Fatalf("pulling %s: %v", replaceTestImage, err)
	}
	return img
}

// startOldApp creates appID's container the way an earlier deploy left it,
// with the given entitlements in its labels, and starts a long-running task.
func startOldApp(t *testing.T, c *Client, ctx context.Context, img containerd.Image, appID string, entitlements []appconfig.Entitlement) {
	t.Helper()
	ctr, err := c.client.NewContainer(ctx, appID,
		containerd.WithImage(img),
		containerd.WithSnapshotter(c.snapshotter),
		containerd.WithNewSnapshot(appID+"-old", img),
		containerd.WithNewSpec(oci.WithImageConfig(img), oci.WithProcessArgs("sleep", "300")),
		containerd.WithContainerLabels(wendyLabels(appID, "", "1", nil, entitlements, "", nil)),
	)
	if err != nil {
		t.Fatalf("creating old container: %v", err)
	}
	task, err := ctr.NewTask(ctx, cio.NullIO)
	if err != nil {
		t.Fatalf("creating old task: %v", err)
	}
	if err := task.Start(ctx); err != nil {
		t.Fatalf("starting old task: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(ctx)
		if task, err := ctr.Task(ctx, nil); err == nil {
			_, _ = task.Delete(ctx, containerd.WithProcessKill)
		}
		_ = ctr.Delete(ctx, containerd.WithSnapshotCleanup)
	})
}

// requireOldAppRunning fails unless appID's container is still the old
// deploy's (version 1) and its task still runs.
func requireOldAppRunning(t *testing.T, c *Client, ctx context.Context, appID string) {
	t.Helper()
	ctr, err := c.client.LoadContainer(ctx, appID)
	if err != nil {
		t.Fatalf("old container is gone: %v", err)
	}
	labels, err := ctr.Labels(ctx)
	if err != nil {
		t.Fatalf("reading container labels: %v", err)
	}
	if got := labels[labelKeyAppVersion]; got != "1" {
		t.Fatalf("container version = %q, want the old deploy's 1", got)
	}
	task, err := ctr.Task(ctx, nil)
	if err != nil {
		t.Fatalf("old task is gone: %v", err)
	}
	st, err := task.Status(ctx)
	if err != nil {
		t.Fatalf("reading old task status: %v", err)
	}
	if st.Status != containerd.Running {
		t.Fatalf("old task status = %s, want running", st.Status)
	}
}

// socketCallLog records, in order, the Ensure and Release calls of both
// socket providers.
type socketCallLog struct{ calls []string }

// listenSocket stands in for the providers' listeners: ApplyEntitlements
// only mounts a socket directory once the socket in it exists.
func listenSocket(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
		return
	}
	l, err := net.Listen("unix", filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("listening on %s: %v", name, err)
	}
	t.Cleanup(func() { _ = l.Close() })
}

type orderedSystemAPIProvider struct {
	t   *testing.T
	log *socketCallLog
	dir string
}

func (p *orderedSystemAPIProvider) Ensure(appID, serviceName string, _ []string) (string, error) {
	p.log.calls = append(p.log.calls, "system-api ensure")
	listenSocket(p.t, p.dir, "system.sock")
	return p.dir, nil
}
func (p *orderedSystemAPIProvider) Release(string, string) {
	p.log.calls = append(p.log.calls, "system-api release")
}
func (p *orderedSystemAPIProvider) ReleaseApp(string) {}

type orderedDataProvider struct {
	t   *testing.T
	log *socketCallLog
	dir string
}

func (p *orderedDataProvider) Ensure(string, string) (string, error) {
	p.log.calls = append(p.log.calls, "data ensure")
	listenSocket(p.t, p.dir, "data.sock")
	return p.dir, nil
}
func (p *orderedDataProvider) Release(string, string) {
	p.log.calls = append(p.log.calls, "data release")
}
func (p *orderedDataProvider) ReleaseApp(string) {}

// shortTempDir returns a directory short enough for a unix socket path,
// which t.TempDir() under a long test name can exceed.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wdy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

var hostSocketEntitlements = []appconfig.Entitlement{
	{Type: appconfig.EntitlementBluetooth},
	{Type: appconfig.EntitlementNotifications},
	{Type: appconfig.EntitlementEpisodeWrite},
}

func TestIntegrationFailedRedeployKeepsRunningApp(t *testing.T) {
	cases := []struct {
		name string
		req  *agentpb.CreateContainerRequest
		cfg  appconfig.AppConfig
	}{
		{
			name: "image cannot be pulled",
			req:  &agentpb.CreateContainerRequest{ImageName: "127.0.0.1:5000/wdy3850/missing:1"},
		},
		{
			name: "reserved env var",
			req:  &agentpb.CreateContainerRequest{ImageName: replaceTestImage, Env: []string{"LD_PRELOAD=/x.so"}},
		},
		{
			name: "malformed memory limit",
			req:  &agentpb.CreateContainerRequest{ImageName: replaceTestImage},
			cfg:  appconfig.AppConfig{Resources: &appconfig.ResourceLimits{Memory: "lots"}},
		},
		{
			name: "bluetooth without a D-Bus proxy",
			req:  &agentpb.CreateContainerRequest{ImageName: replaceTestImage},
			cfg:  appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementBluetooth}}},
		},
		{
			name: "notifications without a System API socket manager",
			req:  &agentpb.CreateContainerRequest{ImageName: replaceTestImage},
			cfg:  appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNotifications}}},
		},
		{
			name: "malformed i2c device",
			req:  &agentpb.CreateContainerRequest{ImageName: replaceTestImage},
			cfg:  appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementI2C, Device: "spi-0"}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ctx := newIntegrationClient(t)
			startOldApp(t, c, ctx, pullReplaceTestImage(t, c, ctx), "replaced-app", nil)

			cfg := tc.cfg
			cfg.AppID = "replaced-app"
			cfg.Version = "2"
			tc.req.AppName = cfg.AppID
			if err := c.CreateContainerWithProgress(ctx, tc.req, &cfg, nil); err == nil {
				t.Fatal("CreateContainerWithProgress succeeded; want the redeploy refused")
			}

			requireOldAppRunning(t, c, ctx, "replaced-app")
		})
	}
}

// A refused redeploy of a bridge-networked app leaves the old container's
// network sandbox in place: tearing it down (CNI DEL, IPAM, DNS) would cut the
// still-running app off the network.
func TestIntegrationFailedRedeployKeepsNetworkSandbox(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	bridge := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "bridge"}}
	startOldApp(t, c, ctx, pullReplaceTestImage(t, c, ctx), "replaced-app", bridge)
	closed := false
	c.networkSandboxes = map[string]*networkSandbox{}
	c.registerNetworkSandbox(&networkSandbox{
		appID:        "replaced-app",
		containerID:  "replaced-app",
		identity:     networkIdentity("", bridge),
		entitlements: bridge,
		cleanup:      func() { closed = true },
	})

	req := &agentpb.CreateContainerRequest{AppName: "replaced-app", ImageName: replaceTestImage, Env: []string{"LD_PRELOAD=/x.so"}}
	cfg := &appconfig.AppConfig{AppID: "replaced-app", Version: "2", Entitlements: bridge}
	if err := c.CreateContainerWithProgress(ctx, req, cfg, nil); err == nil {
		t.Fatal("CreateContainerWithProgress succeeded; want the reserved env var refused")
	}

	if closed || c.takeNetworkSandbox("replaced-app") == nil {
		t.Fatal("the refused redeploy tore down the old container's network sandbox")
	}
	requireOldAppRunning(t, c, ctx, "replaced-app")
}

// A replace pauses the old container's restarts from its start, not only for
// the swap: a restart the monitor began during the image pull would otherwise
// wait on the container's lock and then start the new container behind the
// deploy's back. The pause ends with the create, so a refused redeploy's app
// is restarted as usual if it crashes.
func TestIntegrationRedeployPausesRestartsWhilePreparing(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	monitor := &fakeRestartSuppressor{}
	c.SetRestartSuppressor(monitor)
	startOldApp(t, c, ctx, pullReplaceTestImage(t, c, ctx), "replaced-app", nil)

	req := &agentpb.CreateContainerRequest{AppName: "replaced-app", ImageName: "127.0.0.1:5000/wdy3850/missing:1"}
	cfg := &appconfig.AppConfig{AppID: "replaced-app", Version: "2"}
	if err := c.CreateContainerWithProgress(ctx, req, cfg, nil); err == nil {
		t.Fatal("CreateContainerWithProgress succeeded; want the image pull to fail")
	}

	want := []string{"replaced-app"}
	if !reflect.DeepEqual(monitor.suppressed, want) || !reflect.DeepEqual(monitor.resumed, want) {
		t.Fatalf("restarts suppressed = %v, resumed = %v; want both %v", monitor.suppressed, monitor.resumed, want)
	}
}

// The old container's D-Bus proxy and sockets stay in place when the redeploy
// is refused before the swap: releasing them would cut the still-running app
// off its bluetooth, notifications and data access.
func TestIntegrationFailedRedeployLeavesOldHostSocketsAlone(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	proxy := &recordingProxyManager{dir: shortTempDir(t)}
	log := &socketCallLog{}
	c.proxyManager = proxy
	c.systemAPISocketProvider = &orderedSystemAPIProvider{t: t, log: log, dir: shortTempDir(t)}
	c.dataSocketProvider = &orderedDataProvider{t: t, log: log, dir: shortTempDir(t)}
	startOldApp(t, c, ctx, pullReplaceTestImage(t, c, ctx), "replaced-app", hostSocketEntitlements)

	req := &agentpb.CreateContainerRequest{AppName: "replaced-app", ImageName: replaceTestImage, Env: []string{"LD_PRELOAD=/x.so"}}
	cfg := &appconfig.AppConfig{AppID: "replaced-app", Version: "2", Entitlements: hostSocketEntitlements}
	if err := c.CreateContainerWithProgress(ctx, req, cfg, nil); err == nil {
		t.Fatal("CreateContainerWithProgress succeeded; want the reserved env var refused")
	}

	if len(proxy.started) != 0 || len(proxy.stopped) != 0 {
		t.Fatalf("D-Bus proxy starts = %v, stops = %v; want neither", proxy.started, proxy.stopped)
	}
	if len(log.calls) != 0 {
		t.Fatalf("socket calls = %v; want none", log.calls)
	}
	requireOldAppRunning(t, c, ctx, "replaced-app")
}

// A redeploy releases the old container's sockets before the new container
// claims them (each provider records one owner per service, so the reverse
// order would drop the new claim), and the new container mounts them.
func TestIntegrationRedeployHandsHostSocketsToNewContainer(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	proxyDir := shortTempDir(t)
	systemAPIDir := shortTempDir(t)
	dataDir := shortTempDir(t)
	proxy := &recordingProxyManager{dir: proxyDir}
	log := &socketCallLog{}
	c.proxyManager = proxy
	c.systemAPISocketProvider = &orderedSystemAPIProvider{t: t, log: log, dir: systemAPIDir}
	c.dataSocketProvider = &orderedDataProvider{t: t, log: log, dir: dataDir}
	startOldApp(t, c, ctx, pullReplaceTestImage(t, c, ctx), "replaced-app", hostSocketEntitlements)

	req := &agentpb.CreateContainerRequest{AppName: "replaced-app", ImageName: replaceTestImage}
	cfg := &appconfig.AppConfig{AppID: "replaced-app", Version: "2", Entitlements: hostSocketEntitlements}
	if err := c.CreateContainerWithProgress(ctx, req, cfg, nil); err != nil {
		t.Fatalf("CreateContainerWithProgress: %v", err)
	}

	wantCalls := []string{"system-api release", "data release", "system-api ensure", "data ensure"}
	if !reflect.DeepEqual(log.calls, wantCalls) {
		t.Fatalf("socket calls = %v, want %v", log.calls, wantCalls)
	}
	if !reflect.DeepEqual(proxy.stopped, []string{"replaced-app"}) || !reflect.DeepEqual(proxy.started, []string{"replaced-app"}) {
		t.Fatalf("D-Bus proxy stops = %v, starts = %v; want the old one stopped and a new one started", proxy.stopped, proxy.started)
	}

	ctr, err := c.client.LoadContainer(ctx, "replaced-app")
	if err != nil {
		t.Fatalf("loading new container: %v", err)
	}
	spec, err := ctr.Spec(ctx)
	if err != nil {
		t.Fatalf("reading new container spec: %v", err)
	}
	wantMounts := map[string]string{
		"/var/run/dbus":     proxyDir,
		"/run/wendy/system": systemAPIDir,
		"/run/wendy/data":   dataDir,
	}
	seen := map[string]int{}
	for _, m := range spec.Mounts {
		if want, ok := wantMounts[m.Destination]; ok && m.Source == want {
			seen[m.Destination]++
		}
	}
	for dest := range wantMounts {
		if seen[dest] != 1 {
			t.Errorf("new container mounts %s %d times, want once", dest, seen[dest])
		}
	}
}

// A shared-namespace group's primary that is redeployed must not join the
// namespaces of its own old task, which the swap is about to kill: the group
// join happens only after that task is gone, so the new container becomes the
// primary.
func TestIntegrationRedeployedPrimaryDoesNotJoinItsOldTask(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	startOldApp(t, c, ctx, pullReplaceTestImage(t, c, ctx), "replaced-app", nil)
	ctr, err := c.client.LoadContainer(ctx, "replaced-app")
	if err != nil {
		t.Fatal(err)
	}
	task, err := ctr.Task(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldPID := task.Pid()
	c.primaryPIDs = map[string]uint32{"replaced-app": oldPID}

	req := &agentpb.CreateContainerRequest{AppName: "replaced-app", ImageName: replaceTestImage}
	cfg := &appconfig.AppConfig{AppID: "replaced-app", Version: "2", Isolation: "shared-network"}
	if err := c.CreateContainerWithProgress(ctx, req, cfg, nil); err != nil {
		t.Fatalf("CreateContainerWithProgress: %v", err)
	}

	ctr, err = c.client.LoadContainer(ctx, "replaced-app")
	if err != nil {
		t.Fatalf("loading new container: %v", err)
	}
	spec, err := ctr.Spec(ctx)
	if err != nil {
		t.Fatalf("reading new container spec: %v", err)
	}
	oldNS := "/proc/" + strconv.FormatUint(uint64(oldPID), 10) + "/"
	for _, ns := range spec.Linux.Namespaces {
		if strings.HasPrefix(ns.Path, oldNS) {
			t.Fatalf("new container joins the old task's %s namespace (%s)", ns.Type, ns.Path)
		}
	}
}

func TestIntegrationRedeployReplacesRunningApp(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	startOldApp(t, c, ctx, pullReplaceTestImage(t, c, ctx), "replaced-app", nil)

	req := &agentpb.CreateContainerRequest{AppName: "replaced-app", ImageName: replaceTestImage}
	cfg := &appconfig.AppConfig{AppID: "replaced-app", Version: "2"}
	if err := c.CreateContainerWithProgress(ctx, req, cfg, nil); err != nil {
		t.Fatalf("CreateContainerWithProgress: %v", err)
	}

	ctr, err := c.client.LoadContainer(ctx, "replaced-app")
	if err != nil {
		t.Fatalf("loading new container: %v", err)
	}
	labels, err := ctr.Labels(ctx)
	if err != nil {
		t.Fatalf("reading container labels: %v", err)
	}
	if got := labels[labelKeyAppVersion]; got != "2" {
		t.Fatalf("container version = %q, want the new deploy's 2", got)
	}
	if _, err := ctr.Task(ctx, nil); err == nil {
		t.Fatal("old task survived the replace; it must be killed before the new container starts (WDY-1818)")
	}
}

// Failures that can only happen once the old container is deleted say so, so
// the user knows the app is no longer running.
func TestIntegrationRedeployErrorAfterSwapSaysAppStopped(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	c.proxyManager = &recordingProxyManager{err: errors.New("xdg-dbus-proxy exited")}
	startOldApp(t, c, ctx, pullReplaceTestImage(t, c, ctx), "replaced-app", nil)

	req := &agentpb.CreateContainerRequest{AppName: "replaced-app", ImageName: replaceTestImage}
	cfg := &appconfig.AppConfig{AppID: "replaced-app", Version: "2", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementBluetooth}}}
	err := c.CreateContainerWithProgress(ctx, req, cfg, nil)
	if err == nil {
		t.Fatal("CreateContainerWithProgress succeeded; want the D-Bus proxy failure")
	}
	if !strings.Contains(err.Error(), "xdg-dbus-proxy exited") || !strings.Contains(err.Error(), "no longer running") {
		t.Fatalf("error = %q; want the cause and that the app is no longer running", err)
	}
}

// WDY-3799: layer snapshots that are not stacked on their parents can only be
// replaced once the old container stops using them, so the repair has to wait
// until that container is deleted.
func TestIntegrationRedeployRepairsLayersUnderOldContainer(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	img := newIntegrationImage(t, c, ctx)
	image := assembleIntegrationImage(t, c, ctx, img)
	detachLayers(t, c, ctx, img)
	if _, err := c.client.NewContainer(ctx, "replaced-app",
		containerd.WithImage(image),
		containerd.WithSnapshotter(c.snapshotter),
		containerd.WithNewSnapshot("replaced-app-old", image),
		containerd.WithSpec(&oci.Spec{}),
		containerd.WithContainerLabels(wendyLabels("replaced-app", "", "1", nil, nil, "", nil)),
	); err != nil {
		t.Fatalf("creating old container on the detached layers: %v", err)
	}

	req := &agentpb.CreateContainerRequest{AppName: "replaced-app", ImageName: img.name}
	cfg := &appconfig.AppConfig{AppID: "replaced-app", Version: "2"}
	if err := c.CreateContainerWithProgress(ctx, req, cfg, nil); err != nil {
		t.Fatalf("CreateContainerWithProgress: %v", err)
	}

	ctr, err := c.client.LoadContainer(ctx, "replaced-app")
	if err != nil {
		t.Fatalf("loading new container: %v", err)
	}
	info, err := ctr.Info(ctx)
	if err != nil {
		t.Fatalf("reading new container info: %v", err)
	}
	mounts, err := c.client.SnapshotService(c.snapshotter).Mounts(ctx, info.SnapshotKey)
	if err != nil {
		t.Fatalf("reading new container rootfs mounts: %v", err)
	}
	requireRootfsFiles(t, ctx, mounts)
}

// When another app's container also runs on the unstacked layers, removing
// the old container cannot unblock the WDY-3799 repair, so the redeploy is
// refused while the old container is still in place.
func TestIntegrationRedeployRefusedWhenAnotherAppBlocksRepair(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	img := newIntegrationImage(t, c, ctx)
	image := assembleIntegrationImage(t, c, ctx, img)
	detachLayers(t, c, ctx, img)
	lctx, done, err := c.client.WithLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.client.SnapshotService(c.snapshotter).Prepare(lctx, "wendy-prepared-sibling", img.topChainID()); err != nil {
		t.Fatalf("preparing sibling rootfs: %v", err)
	}
	if _, err := c.client.NewContainer(lctx, "sibling-app",
		containerd.WithSnapshotter(c.snapshotter),
		containerd.WithSnapshot("wendy-prepared-sibling"),
		containerd.WithSpec(&oci.Spec{}),
	); err != nil {
		t.Fatalf("creating sibling container: %v", err)
	}
	_ = done(context.WithoutCancel(ctx))
	if _, err := c.client.NewContainer(ctx, "replaced-app",
		containerd.WithImage(image),
		containerd.WithSnapshotter(c.snapshotter),
		containerd.WithNewSnapshot("replaced-app-old", image),
		containerd.WithSpec(&oci.Spec{}),
		containerd.WithContainerLabels(wendyLabels("replaced-app", "", "1", nil, nil, "", nil)),
	); err != nil {
		t.Fatalf("creating old container on the detached layers: %v", err)
	}

	req := &agentpb.CreateContainerRequest{AppName: "replaced-app", ImageName: img.name}
	cfg := &appconfig.AppConfig{AppID: "replaced-app", Version: "2"}
	err = c.CreateContainerWithProgress(ctx, req, cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "sibling-app") {
		t.Fatalf("CreateContainerWithProgress error = %v; want it to name sibling-app", err)
	}

	ctr, err := c.client.LoadContainer(ctx, "replaced-app")
	if err != nil {
		t.Fatalf("old container is gone: %v", err)
	}
	labels, err := ctr.Labels(ctx)
	if err != nil {
		t.Fatalf("reading container labels: %v", err)
	}
	if got := labels[labelKeyAppVersion]; got != "1" {
		t.Fatalf("container version = %q, want the old deploy's 1", got)
	}
}
