//go:build linux

package containerd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/wendylabsinc/wendy/go/internal/agent/board"
	"github.com/wendylabsinc/wendy/go/internal/agent/cdi"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// #1666 / #2828: exercise production creation and restart against real
// containerd/runc. Only board detection and the vendor CDI document are fixtures.
// Reading mounted files inside the task proves runtime provisioning, but does
// not claim that a vendor EGL/CUDA driver works on physical hardware.
func TestIntegrationDisplayCDIMountsAndRestart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    board.Kind
		display bool
		wantCDI bool
	}{
		{"jetson-display", board.Jetson, true, true},
		{"jetson-unentitled", board.Jetson, false, false},
		{"generic-display", board.Generic, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ctx := newIntegrationClient(t)
			stubBoard(t, tc.kind)
			createReplaceTestImage(t, c, ctx)
			directory := t.TempDir()
			global := filepath.Join(directory, "vendor-global.json")
			old := filepath.Join(directory, "libfixture.so.1")
			updated := filepath.Join(directory, "libfixture.so.2")
			for path, content := range map[string]string{global: "global", old: "driver-v1", updated: "driver-v2"} {
				if err := os.WriteFile(path, []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			mount := func(source, destination string) cdi.CDIMount {
				return cdi.CDIMount{HostPath: source, ContainerPath: destination, Type: "bind", Options: []string{"bind", "ro"}}
			}
			vendor := &cdi.CDISpecification{
				CDIVersion: "0.5.0", Kind: "nvidia.com/gpu",
				ContainerEdits: &cdi.CDIContainerEdits{Mounts: []cdi.CDIMount{mount(global, "/vendor-global.json")}},
				Devices: []cdi.CDIDevice{{Name: "all", ContainerEdits: cdi.CDIContainerEdits{
					Mounts: []cdi.CDIMount{mount(old, "/libfixture.so.1")}, Env: []string{"FIXTURE_VENDOR_GENERATION=one"},
				}}},
			}
			previous := loadNvidiaCDISpec
			loads := 0
			loadNvidiaCDISpec = func() (*cdi.CDISpecification, string, error) {
				loads++
				return vendor, filepath.Join(directory, "nvidia.yaml"), nil
			}
			t.Cleanup(func() { loadNvidiaCDISpec = previous })
			cfg := &appconfig.AppConfig{AppID: tc.name, Version: "1"}
			if tc.display {
				cfg.Entitlements = []appconfig.Entitlement{{Type: appconfig.EntitlementDisplay}}
			}
			if err := c.CreateContainerWithProgress(ctx, &agentpb.CreateContainerRequest{AppName: tc.name, ImageName: replaceTestImage}, cfg, nil); err != nil {
				t.Fatal(err)
			}
			ctr, err := c.client.LoadContainer(ctx, tc.name)
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
			before, err := ctr.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			assertSpec := func(source, destination, generation string) {
				t.Helper()
				spec, err := ctr.Spec(ctx)
				if err != nil {
					t.Fatal(err)
				}
				seen := map[string]int{}
				for _, m := range spec.Mounts {
					if m.Destination != "/vendor-global.json" && m.Destination != "/libfixture.so.1" && m.Destination != "/libfixture.so.2" {
						continue
					}
					wantSource := source
					if m.Destination == "/vendor-global.json" {
						wantSource = global
					} else if m.Destination != destination {
						t.Fatalf("stale CDI mount: %+v", m)
					}
					if !tc.wantCDI || m.Source != wantSource || m.Type != "bind" || !slices.Contains(m.Options, "ro") {
						t.Fatalf("unexpected CDI mount: %+v", m)
					}
					seen[m.Destination]++
				}
				if tc.wantCDI && (seen["/vendor-global.json"] != 1 || seen[destination] != 1) {
					t.Fatalf("display CDI mounts missing or duplicated: %v", seen)
				}
				if slices.Contains(spec.Process.Env, "FIXTURE_VENDOR_GENERATION="+generation) != tc.wantCDI {
					t.Fatalf("CDI environment mismatch: %v", spec.Process.Env)
				}
			}
			assertSpec(old, "/libfixture.so.1", "one")
			run := func(script, want string) {
				t.Helper()
				runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
				stream, err := c.StartContainer(runCtx, tc.name, "", nil)
				if err != nil {
					t.Fatalf("starting CDI fixture: %v", err)
				}
				drained := make(chan struct{})
				go func() {
					for range stream {
					}
					close(drained)
				}()
				var stdout, stderr bytes.Buffer
				code, err := c.ExecInContainer(runCtx, tc.name, []string{"/bin/sh", "-ec", script}, false, nil, &stdout, &stderr, nil)
				if err != nil || code != 0 || stdout.String() != want {
					t.Fatalf("runtime CDI probe: exit=%d err=%v stdout=%q stderr=%q", code, err, &stdout, &stderr)
				}
				if err := c.StopContainer(runCtx, tc.name); err != nil {
					t.Fatal(err)
				}
				select {
				case <-drained:
				case <-runCtx.Done():
					t.Fatal("CDI fixture output did not drain")
				}
			}
			if !tc.wantCDI {
				run("test ! -e /vendor-global.json; test ! -e /libfixture.so.1; test -z \"$FIXTURE_VENDOR_GENERATION\"; printf absent", "absent")
				if loads != 0 {
					t.Fatalf("unentitled/non-Jetson app loaded NVIDIA CDI %d times", loads)
				}
				return
			}
			run("/bin/busybox cat /vendor-global.json /libfixture.so.1; printf %s \"$FIXTURE_VENDOR_GENERATION\"", "globaldriver-v1one")
			if err := os.Remove(old); err != nil {
				t.Fatal(err)
			}
			vendor.Devices[0].ContainerEdits.Mounts = []cdi.CDIMount{mount(updated, "/libfixture.so.2")}
			vendor.Devices[0].ContainerEdits.Env = []string{"FIXTURE_VENDOR_GENERATION=two"}
			run("test ! -s /libfixture.so.1; /bin/busybox cat /vendor-global.json /libfixture.so.2; printf %s \"$FIXTURE_VENDOR_GENERATION\"", "globaldriver-v2two")
			assertSpec(updated, "/libfixture.so.2", "two")
			after, err := ctr.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if before.ID != after.ID || before.SnapshotKey != after.SnapshotKey || !before.CreatedAt.Equal(after.CreatedAt) {
				t.Fatal("restart replaced the container instead of repairing its stored spec")
			}
		})
	}
}
