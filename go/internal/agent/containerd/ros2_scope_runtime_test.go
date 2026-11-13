//go:build linux

package containerd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestROS2ScopeRuntimeVisibility(t *testing.T) {
	if os.Getenv("WENDY_ROS_SCOPE_NAMESPACE") != "1" {
		t.Skip("requires the private hosted ROS scope runner")
	}
	for _, kind := range []string{"mnt", "net", "pid"} {
		current, err := os.Readlink("/proc/self/ns/" + kind)
		parent := os.Getenv("WENDY_ROS_SCOPE_PARENT_" + kind)
		if err != nil || parent == "" || current == parent {
			t.Fatalf("%s namespace is not private", kind)
		}
	}
	c, ctx := newIntegrationClient(t)
	archive, err := os.Open(os.Getenv("WENDY_ROS_SCOPE_ARCHIVE"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.client.Import(ctx, archive)
	archive.Close()
	if err != nil {
		t.Fatal(err)
	}
	image := os.Getenv("WENDY_ROS_SCOPE_IMAGE")
	nonce := c.namespace
	domain := 61
	type running struct {
		container      containerd.Container
		task           containerd.Task
		stdout, stderr *bytes.Buffer
	}
	start := func(name, scope, role, network string) *running {
		t.Helper()
		appID := nonce + "-" + name
		cfg := &appconfig.AppConfig{AppID: appID, Version: "1", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "host"}}, Frameworks: &appconfig.FrameworksConfig{ROS2: &appconfig.ROS2Config{Scope: scope, DomainID: &domain}}}
		mode := "observe"
		if role != "" {
			mode = "publish"
		}
		command := fmt.Sprintf("source /opt/ros/humble/setup.bash && exec python3 /scope.py %s --role %s --nonce %s", mode, role, nonce)
		if role == "" {
			command = fmt.Sprintf("source /opt/ros/humble/setup.bash && exec python3 /scope.py observe --nonce %s", nonce)
		}
		req := &agentpb.CreateContainerRequest{AppName: appID, ImageName: image, Cmd: "/bin/bash", UserArgs: []string{"-c", command}}
		if err := c.CreateContainerWithProgress(ctx, req, cfg, nil); err != nil {
			t.Fatal(err)
		}
		ctr, err := c.client.LoadContainer(ctx, appID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer done()
			_ = ctr.Delete(cleanup, containerd.WithSnapshotCleanup)
		})
		if network != "" {
			// Only the external protocol peers get fixture network namespaces. Both
			// candidate scopes and the device app use production host-network setup.
			spec, err := ctr.Spec(ctx)
			if err != nil {
				t.Fatal(err)
			}
			spec.Linux.Namespaces = append(spec.Linux.Namespaces, specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: "/run/netns/" + network})
			if err := ctr.Update(ctx, containerd.UpdateContainerOpts(containerd.WithSpec(spec))); err != nil {
				t.Fatal(err)
			}
		}
		out, errout := &bytes.Buffer{}, &bytes.Buffer{}
		task, err := ctr.NewTask(ctx, cio.NewCreator(cio.WithStreams(nil, out, errout)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer done()
			_, _ = task.Delete(cleanup, containerd.WithProcessKill)
		})
		if err := task.Start(ctx); err != nil {
			t.Fatal(err)
		}
		expected := "/proc/self/ns/net"
		if network != "" {
			expected = "/run/netns/" + network
		}
		wanted, err := os.Stat(expected)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := os.Stat(fmt.Sprintf("/proc/%d/ns/net", task.Pid()))
		if err != nil || !os.SameFile(wanted, actual) {
			t.Fatalf("%s escaped its intended namespace: %v", name, err)
		}
		return &running{ctr, task, out, errout}
	}
	publishers := []*running{start("scope-device", "device", "device", ""), start("scope-uplink", "network", "uplink", "uplink"), start("scope-peripheral", "network", "peripheral", "peripheral")}
	observe := func(name, scope string, want, forbidden []string) {
		t.Helper()
		run := start(name, scope, "", "")
		waitCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		done, err := run.task.Wait(waitCtx)
		if err != nil {
			t.Fatal(err)
		}
		var exit containerd.ExitStatus
		select {
		case exit = <-done:
		case <-waitCtx.Done():
			t.Fatal("DDS observer exceeded deadline")
		}
		code, _, err := exit.Result()
		run.task.IO().Wait()
		if err != nil || code != 0 {
			t.Fatalf("DDS observer exited %d: %v stdout=%s stderr=%s", code, err, run.stdout, run.stderr)
		}
		var received map[string][]int
		if err := json.Unmarshal(bytes.TrimSpace(run.stdout.Bytes()), &received); err != nil {
			t.Fatalf("DDS output: %v %s", err, run.stdout)
		}
		allowed := map[string]bool{}
		excluded := map[string]bool{}
		for _, role := range forbidden {
			excluded[role] = true
		}
		for _, role := range want {
			allowed[role] = true
		}
		for _, role := range []string{"device", "peripheral", "uplink"} {
			if allowed[role] && len(received[role]) < 3 {
				t.Fatalf("%s missing fresh %s payloads: %v", name, role, received)
			}
			if excluded[role] && len(received[role]) != 0 {
				t.Fatalf("%s leaked excluded %s payloads: %v", name, role, received)
			}
		}
		for _, publisher := range publishers {
			state, err := publisher.task.Status(ctx)
			if err != nil || state.Status != containerd.Running {
				t.Fatalf("publisher stopped, cannot prove isolation: %v %v", state, err)
			}
		}
		t.Logf("%s received fresh DDS payloads: device=%d peripheral=%d uplink=%d", name, len(received["device"]), len(received["peripheral"]), len(received["uplink"]))
	}
	// Both positive external paths are observed before any negative isolation
	// claim; publishers remain live for the whole journey.
	observe("scope-network", "network", []string{"device", "uplink"}, nil)
	observe("scope-cable", "peripherals", []string{"device", "peripheral"}, []string{"uplink"})
	// An unreadable or oversized IPv6 route observation is unknown, not an
	// empty route table. Production PeripheralInterfaces must narrow to device.
	view := filepath.Join(os.Getenv("WENDY_ROS_SCOPE_WORK"), "route-view")
	if err := os.MkdirAll(view, 0700); err != nil {
		t.Fatal(err)
	}
	route, err := os.ReadFile("/proc/net/route")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view, "route"), route, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(view, "ipv6_route"), 0700); err != nil {
		t.Fatal(err)
	}
	mount := func(args ...string) {
		t.Helper()
		call, done := context.WithTimeout(ctx, 5*time.Second)
		defer done()
		if output, err := exec.CommandContext(call, "mount", args...).CombinedOutput(); err != nil {
			t.Fatalf("private route mount: %v %s", err, output)
		}
	}
	routeTarget := fmt.Sprintf("/proc/%d/net", os.Getpid())
	mount("--bind", view, routeTarget)
	defer func() {
		call, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer done()
		if output, err := exec.CommandContext(call, "umount", routeTarget).CombinedOutput(); err != nil {
			t.Errorf("unmount private route view: %v %s", err, output)
		}
	}()
	observe("scope-unreadable-v6", "peripherals", []string{"device"}, []string{"peripheral", "uplink"})
	if err := os.Remove(filepath.Join(view, "ipv6_route")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view, "ipv6_route"), bytes.Repeat([]byte("0"), 70000), 0600); err != nil {
		t.Fatal(err)
	}
	observe("scope-oversized-v6", "peripherals", []string{"device"}, []string{"peripheral", "uplink"})
}
