//go:build linux

package containerd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Real middleware/OCI boundary of95f6c1c52. The controlled publisher uses the
// shipped Go2 Cyclone file; this does not execute MuJoCo or fake simulator status.
func TestROSObserverHostRuntime(t *testing.T) {
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
	prepared := os.Getenv("WENDY_ROS_SCOPE_PREPARED")
	configBytes, err := os.ReadFile(filepath.Join(prepared, "context", "observer-wendy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var original appconfig.AppConfig
	if err = json.Unmarshal(configBytes, &original); err != nil || original.Frameworks == nil || original.Frameworks.ROS2 == nil {
		t.Fatalf("actual observer config: %v", err)
	}
	if original.Frameworks.ROS2.DomainID == nil || *original.Frameworks.ROS2.DomainID != 0 {
		t.Fatal("actual observer domain changed; review production source contract")
	}
	expectedHashes := map[string]string{}
	for _, name := range []string{"production-observe.py", "observer-wendy.json", "go2-cyclonedds.xml"} {
		data, err := os.ReadFile(filepath.Join(prepared, "context", name))
		if err != nil {
			t.Fatal(err)
		}
		expectedHashes[name] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	type running struct {
		ctr            containerd.Container
		task           containerd.Task
		done           <-chan containerd.ExitStatus
		stdout, stderr *bytes.Buffer
		exited         bool
	}
	start := func(label, mode string, variant string) *running {
		t.Helper()
		var cfg appconfig.AppConfig
		if err = json.Unmarshal(configBytes, &cfg); err != nil {
			t.Fatal(err)
		}
		cfg.AppID = c.namespace + "-" + label // unique ownership only; explicit domain0 stays exact
		if variant == "legacy-app" {
			cfg.Frameworks.ROS2.Scope = ""
			cfg.Frameworks.ROS2.DiscoveryScope = "app"
		}
		if variant == "other-domain" {
			domain := 1 // Explicit isolation control; not claimed to be original code.
			cfg.Frameworks.ROS2.DomainID = &domain
		}
		command := "source /opt/ros/humble/setup.bash && exec python3 /observer-transport.py observe --nonce \"$1\""
		if mode == "publish" {
			// This is the actual Go2 host publisher's DDS configuration, independent
			// of observer discovery policy. No alternate fake transport is inserted.
			command = "source /opt/ros/humble/setup.bash && export ROS_DOMAIN_ID=0 ROS_LOCALHOST_ONLY=0 CYCLONEDDS_URI=file:///go2-cyclonedds.xml RMW_IMPLEMENTATION=rmw_cyclonedds_cpp && exec python3 /observer-transport.py publish --nonce \"$1\""
		}
		req := &agentpb.CreateContainerRequest{AppName: cfg.AppID, ImageName: os.Getenv("WENDY_ROS_SCOPE_IMAGE"), Cmd: "/bin/bash", UserArgs: []string{"-c", command, "observer-fixture", c.namespace}}
		if err = c.CreateContainerWithProgress(ctx, req, &cfg, nil); err != nil {
			t.Fatal(err)
		}
		ctr, err := c.client.LoadContainer(ctx, cfg.AppID)
		if err != nil {
			t.Fatal(err)
		}
		out, errout := &bytes.Buffer{}, &bytes.Buffer{}
		task, err := ctr.NewTask(ctx, cio.NewCreator(cio.WithStreams(nil, out, errout)))
		if err != nil {
			t.Fatal(err)
		}
		wait, err := task.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		result := &running{ctr: ctr, task: task, done: wait, stdout: out, stderr: errout}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if !result.exited {
				_ = task.Kill(cleanup, syscall.SIGKILL, containerd.WithKillAll)
				select {
				case <-wait:
					result.exited = true
				case <-cleanup.Done():
					t.Error("observer task drain unknown; private namespace runner must reap it")
					return
				}
			}
			task.IO().Wait()
			if _, err := task.Delete(cleanup); err != nil {
				t.Errorf("observer task cleanup: %v", err)
				return
			}
			if err := ctr.Delete(cleanup, containerd.WithSnapshotCleanup); err != nil {
				t.Errorf("observer container cleanup: %v", err)
			}
		})
		if err = task.Start(ctx); err != nil {
			t.Fatal(err)
		}
		want, err := os.Stat("/proc/self/ns/net")
		if err != nil {
			t.Fatal(err)
		}
		actual, err := os.Stat(fmt.Sprintf("/proc/%d/ns/net", task.Pid()))
		if err != nil || !os.SameFile(want, actual) {
			t.Fatal("production observer network entitlement did not join the private host network", err)
		}
		return result
	}
	wait := func(run *running) (uint32, string, string) {
		t.Helper()
		select {
		case exit := <-run.done:
			run.exited = true
			code, _, err := exit.Result()
			if err != nil {
				t.Fatal(err)
			}
			run.task.IO().Wait()
			return code, run.stdout.String(), run.stderr.String()
		case <-time.After(15 * time.Second):
			t.Fatal("actual observer exceeded bounded middleware window")
		}
		return 0, "", ""
	}
	publisher := start("host-odometry", "publish", "normal")
	var previousCounter float64
	for attempt, variant := range []string{"normal", "legacy-app", "other-domain", "normal"} {
		observer := start(fmt.Sprintf("%s-%d", variant, attempt), "observe", variant)
		code, out, errout := wait(observer)
		var report struct {
			Event        string            `json:"event"`
			Passed       bool              `json:"passed"`
			Nonce        string            `json:"nonce"`
			Counters     []float64         `json:"counters"`
			SourceHashes map[string]string `json:"source_sha256"`
		}
		if err = json.Unmarshal(bytes.TrimSpace([]byte(out)), &report); err != nil || report.Event != "production_observer_result" || report.Nonce != c.namespace {
			t.Fatalf("actual observer output %s stderr=%s error=%v", out, errout, err)
		}
		for file, hash := range expectedHashes {
			if report.SourceHashes[file] != hash {
				t.Fatalf("executed observer/source identity changed: %s", file)
			}
		}
		if variant == "other-domain" || (variant == "legacy-app" && !report.Passed) {
			if code != 1 || report.Passed || !strings.Contains(errout, "OBSERVER_HOST_ODOMETRY: production observer did not receive fresh owned host DDS") {
				t.Fatalf("observer exclusion did not fail its actual middleware boundary: code=%d output=%s stderr=%s", code, out, errout)
			}
			if variant == "other-domain" {
				if len(report.Counters) != 0 {
					t.Fatal("observer received foreign-domain DDS payloads")
				}
				t.Log("ROS_OBSERVER_DOMAIN_ISOLATION_VERIFIED: real other-domain observer received no owned source")
			} else {
				t.Log("ROS_OBSERVER_LEGACY_APP_REJECTED: observed historical config sensitivity on this middleware")
			}
		} else {
			if code != 0 || !report.Passed || len(report.Counters) < 3 {
				t.Fatalf("ROS_OBSERVER_HOST_ODOMETRY: actual production observer failed: code=%d output=%s stderr=%s", code, out, errout)
			}
			for i := 1; i < len(report.Counters); i++ {
				if report.Counters[i] <= report.Counters[i-1] {
					t.Fatal("observer DDS payload did not advance")
				}
			}
			if attempt == 3 && report.Counters[0] <= previousCounter {
				t.Fatal("post-control production observer did not receive a fresh publisher generation")
			}
			previousCounter = report.Counters[len(report.Counters)-1]
			if variant == "legacy-app" {
				t.Log("ROS_OBSERVER_LEGACY_APP_COMPATIBLE: current middleware accepts original config; NOT a passing historical negative control")
			}
			t.Logf("ROS_OBSERVER_HOST_ODOMETRY_VERIFIED count=%d nonce=%s", len(report.Counters), report.Nonce)
		}
		state, err := publisher.task.Status(ctx)
		if err != nil || state.Status != containerd.Running {
			t.Fatal("controlled host DDS publisher stopped; cannot prove discovery exclusion", err)
		}
	}
	if err = publisher.task.Kill(ctx, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	_, out, errout := wait(publisher)
	var source struct {
		Event, Nonce string
		Published    int
	}
	if err = json.Unmarshal(bytes.TrimSpace([]byte(out)), &source); err != nil || source.Event != "controlled_publisher_complete" || source.Nonce != c.namespace || source.Published < 20 {
		t.Fatalf("controlled publisher receipts incomplete: %s stderr=%s err=%v", out, errout, err)
	}
	t.Log("ROS_OBSERVER_HOST_RUNTIME_VERIFIED: actual OCI/production Observer + domain isolation + separately classified historical config; no physics")
}
