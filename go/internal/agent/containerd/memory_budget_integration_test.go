//go:build linux

package containerd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// #2453: exercise the kernel budget with anonymous pages, not just OCI fields.
// The launcher provides swap on a disposable hosted runner. A control must
// actually swap pages, so a machine without usable swap cannot give a false pass.
func TestContainerCombinedMemoryBudget(t *testing.T) {
	workload := os.Getenv("WENDY_MEMORY_BUDGET_WORKLOAD")
	if workload == "" {
		t.Skip("requires test-memory-budget.sh on a disposable Linux runner")
	}
	if !filepath.IsAbs(workload) {
		t.Fatal("memory workload must have an absolute path")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Fatalf("fixture requires cgroup v2: %v", err)
	}
	c, ctx := newIntegrationClient(t)
	createReplaceTestImage(t, c, ctx)
	for _, tc := range []struct {
		name      string
		mib       int
		allowSwap bool
		wantOOM   bool
	}{
		{"swap-control", 96, true, false},
		{"within-budget", 16, false, false},
		{"over-budget", 96, false, true},
	} {
		if !t.Run(tc.name, func(t *testing.T) {
			cfg := &appconfig.AppConfig{AppID: tc.name, Version: "1", Resources: &appconfig.ResourceLimits{Memory: "64Mi"}}
			if err := c.CreateContainerWithProgress(ctx, &agentpb.CreateContainerRequest{AppName: tc.name, ImageName: replaceTestImage}, cfg, nil); err != nil {
				t.Fatal(err)
			}
			ctr, err := c.client.LoadContainer(ctx, tc.name)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
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
			// Only supply the fixture executable/arguments. The two regression
			// cases retain the resource settings from production container creation.
			spec.Process.Args = []string{"/memory-workload", strconv.Itoa(tc.mib)}
			spec.Mounts = append(spec.Mounts, specs.Mount{Destination: "/memory-workload", Source: workload, Type: "bind", Options: []string{"bind", "ro"}})
			if tc.allowSwap {
				// Explicit positive control: 64 MiB RAM plus 64 MiB swap.
				combined := int64(128 << 20)
				spec.Linux.Resources.Memory.Swap = &combined
			}
			if err := ctr.Update(ctx, containerd.UpdateContainerOpts(containerd.WithSpec(spec))); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			task, err := ctr.NewTask(ctx, cio.NewCreator(cio.WithStreams(nil, &stdout, &stderr)))
			if err != nil {
				t.Fatal(err)
			}
			group := memoryBudgetCgroup(t, task.Pid())
			if got := memoryBudgetCounter(t, group, "memory.max", ""); got != 64<<20 {
				t.Fatalf("workload RAM is not bounded: memory.max=%d", got)
			}
			before := memoryBudgetCounter(t, group, "memory.events", "oom_kill")
			runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			exited, err := task.Wait(runCtx)
			if err != nil {
				t.Fatal(err)
			}
			if err := task.Start(runCtx); err != nil {
				t.Fatal(err)
			}
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			var peakSwap int64
			var code uint32
		waiting:
			for {
				select {
				case status := <-exited:
					code, _, err = status.Result()
					if err != nil {
						t.Fatal(err)
					}
					break waiting
				case <-ticker.C:
					peakSwap = max(peakSwap, memoryBudgetCounter(t, group, "memory.swap.current", ""))
				case <-runCtx.Done():
					t.Fatal("bounded memory workload did not exit")
				}
			}
			task.IO().Wait()
			kills := memoryBudgetCounter(t, group, "memory.events", "oom_kill") - before
			t.Logf("exit=%d oom_kills=%d peak_swap=%d stdout=%s stderr=%s", code, kills, peakSwap, &stdout, &stderr)
			completed := strings.Contains(stdout.String(), fmt.Sprintf("touched=%d checksum=", tc.mib<<20))
			if tc.wantOOM {
				if code == 0 && kills == 0 && completed {
					t.Fatalf("combined memory budget was exceeded without OOM enforcement: peak_swap=%d stdout=%s", peakSwap, &stdout)
				}
				if code != 137 || kills < 1 || strings.Contains(stdout.String(), "touched=") {
					t.Fatalf("workload did not demonstrate budget OOM: exit=%d oom_kills=%d stdout=%s stderr=%s", code, kills, &stdout, &stderr)
				}
			} else {
				if code != 0 || kills != 0 || !completed {
					t.Fatalf("control workload failed: exit=%d oom_kills=%d stdout=%s stderr=%s", code, kills, &stdout, &stderr)
				}
				if tc.allowSwap && peakSwap == 0 {
					t.Fatal("swap control never used swap; enforcement evidence is inconclusive")
				}
			}
		}) {
			return
		}
	}
}

func memoryBudgetCgroup(t *testing.T, pid uint32) string {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if suffix, ok := strings.CutPrefix(line, "0::/"); ok {
			group := filepath.Join("/sys/fs/cgroup", suffix)
			if !strings.HasPrefix(group, "/sys/fs/cgroup/") {
				t.Fatalf("invalid task cgroup %q", group)
			}
			return group
		}
	}
	t.Fatalf("missing unified task cgroup: %s", data)
	return ""
}

func memoryBudgetCounter(t *testing.T, group, file, key string) int64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(group, file))
	if err != nil {
		t.Fatal(err)
	}
	value := strings.TrimSpace(string(data))
	if key != "" {
		value = ""
		for _, line := range strings.Split(string(data), "\n") {
			if fields := strings.Fields(line); len(fields) == 2 && fields[0] == key {
				value = fields[1]
			}
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("invalid %s %s: %q", file, key, data)
	}
	return n
}
