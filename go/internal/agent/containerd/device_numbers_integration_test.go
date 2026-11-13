//go:build linux

package containerd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// #2540 / #1888: a persisted NPU grant must refresh both the device and its
// cgroup permission. Private nodes use the kernel's tun/loop-control devices, so this
// proves real runtime enforcement without pretending to execute a vendor NPU.
func TestIntegrationNPUDeviceNumberRestart(t *testing.T) {
	c, ctx := newIntegrationClient(t)
	createReplaceTestImage(t, c, ctx)
	const app = "npu-renumber"
	const destination = "/dev/regression-accelerator"
	source := filepath.Join(t.TempDir(), "accelerator")
	if err := unix.Mknod(source, unix.S_IFCHR|0600, int(unix.Mkdev(10, 200))); err != nil {
		t.Fatal(err)
	}
	cfg := &appconfig.AppConfig{AppID: app, Version: "1", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNPU}}}
	if err := c.CreateContainerWithProgress(ctx, &agentpb.CreateContainerRequest{AppName: app, ImageName: replaceTestImage, Cmd: "/bin/sh", UserArgs: []string{"-c", "trap 'exit 0' TERM; while :; do sleep 1; done"}}, cfg, nil); err != nil {
		t.Fatal(err)
	}
	ctr, err := c.client.LoadContainer(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if task, err := ctr.Task(cleanupCtx, nil); err == nil {
			_, _ = task.Delete(cleanupCtx, containerd.WithProcessKill)
		}
		_ = ctr.Delete(cleanupCtx, containerd.WithSnapshotCleanup)
	})
	spec, err := ctr.Spec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0600)
	spec.Linux.Devices = append(spec.Linux.Devices, specs.LinuxDevice{Path: destination, Type: "c", Major: 10, Minor: 200, FileMode: &mode})
	// Neither device is in the standard OCI /dev/null/zero grants. Only the
	// old pair is granted; restart must add access for the new pair itself.
	major, minor := int64(10), int64(200)
	spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices,
		specs.LinuxDeviceCgroup{Allow: true, Type: "c", Major: &major, Minor: &minor, Access: "rw"})
	pins, err := json.Marshal([]localoci.PinnedDevice{{Path: destination, HostPath: source, Type: "c", Major: 10, Minor: 200, NoFollow: true}})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Annotations == nil {
		spec.Annotations = map[string]string{}
	}
	spec.Annotations["sh.wendy/pinned-devices"] = string(pins)
	if err := ctr.Update(ctx, containerd.UpdateContainerOpts(containerd.WithSpec(spec))); err != nil {
		t.Fatal(err)
	}
	before, err := ctr.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run := func(want string) {
		t.Helper()
		runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		stream, err := c.StartContainer(runCtx, app, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		drained := make(chan struct{})
		go func() {
			for range stream {
			}
			close(drained)
		}()
		var stdout, stderr bytes.Buffer
		code, err := c.ExecInContainer(runCtx, app, []string{"/bin/sh", "-ec", "/bin/busybox dd if=/dev/regression-accelerator of=/dev/null count=0 2>/dev/null; /bin/busybox stat -c %t:%T /dev/regression-accelerator"}, false, nil, &stdout, &stderr, nil)
		if err != nil || code != 0 || stdout.String() != want {
			t.Fatalf("device number refresh did not reach runtime: exit=%d err=%v stdout=%q stderr=%q want=%s", code, err, &stdout, &stderr, want)
		}
		if err := c.StopContainer(runCtx, app); err != nil {
			t.Fatal(err)
		}
		select {
		case <-drained:
		case <-runCtx.Done():
			t.Fatal("device fixture output did not drain")
		}
	}
	run("a:c8\n")
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mknod(source, unix.S_IFCHR|0600, int(unix.Mkdev(10, 237))); err != nil {
		t.Fatal(err)
	}
	run("a:ed\n")
	after, err := ctr.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.ID != after.ID || before.SnapshotKey != after.SnapshotKey || !before.CreatedAt.Equal(after.CreatedAt) {
		t.Fatal("device repair recreated the container")
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	_, err = c.StartContainer(ctx, app, "", nil)
	if !errors.Is(err, ErrDeviceUnavailable) {
		t.Fatalf("missing device start error = %v; want ErrDeviceUnavailable", err)
	}
	t.Logf("missing-device diagnostic for %s at %s: %v", app, source, err)
	if task, err := ctr.Task(ctx, nil); err == nil {
		status, err := task.Status(ctx)
		if err != nil || status.Status == containerd.Running {
			t.Fatalf("missing device left a running task: %+v, %v", status, err)
		}
	}
}
