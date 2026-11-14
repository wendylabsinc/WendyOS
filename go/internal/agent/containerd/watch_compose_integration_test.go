//go:build linux

package containerd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/stats"
)

// Actual Compose watch and prebuilt-image deployment. The image is assembled
// locally; Docker/buildkit source builds and physical devices are not involved.
func TestWatchComposeContainerSession(t *testing.T) {
	if os.Getenv("WENDY_WATCH_CONTAINER_NAMESPACE") != "1" {
		t.Skip("requires the private hosted container watch runner")
	}
	for _, kind := range []string{"mnt", "net", "pid"} {
		current, err := os.Readlink("/proc/self/ns/" + kind)
		parent := os.Getenv("WENDY_WATCH_PARENT_" + kind)
		if err != nil || parent == "" || current == parent {
			t.Fatalf("%s namespace is not private", kind)
		}
	}
	cli := os.Getenv("WENDY_WATCH_CLI")
	if !filepath.IsAbs(cli) {
		t.Fatal("require an exact built CLI")
	}
	c, ctx := newIntegrationClient(t)
	image := createReplaceTestImage(t, c, ctx)
	imageName := "docker.io/wendy-test/watch@" + image.Target().Digest.String()
	if _, err := c.client.ImageService().Create(ctx, images.Image{Name: imageName, Target: image.Target()}); err != nil {
		t.Fatal(err)
	}
	work := os.Getenv("WENDY_WATCH_WORK")
	if !filepath.IsAbs(work) || !strings.HasPrefix(filepath.Base(work), "wendy-watch-container.") {
		t.Fatal("require the outer owned namespace work directory")
	}
	// The outer launcher removes this only after namespace init is reaped.
	// Testing.TempDir would remove it even if CLI cleanup could not join.
	root, err := os.MkdirTemp(work, "session-")
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "watch-"+c.namespace)
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	appID := filepath.Base(project)
	worker, stable := appID+"_worker", appID+"_stable"
	for _, app := range []string{worker, stable} {
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if err := c.DeleteContainer(cleanup, app, false); err != nil {
				t.Errorf("cleanup %s: %v", app, err)
			}
		})
	}
	writeCompose := func(value string) {
		t.Helper()
		// JSON is also valid YAML. The real parser receives a complete compose
		// document on each save; no watcher/build/deploy function is replaced.
		service := func(role, value string) map[string]any {
			return map[string]any{
				"image": imageName, "network_mode": "none",
				"environment": map[string]string{"VALUE": value, "ROLE": role, "OVERRIDE": "compose"},
				"command":     []string{"/bin/sh", "-c", `sleep 20; n=0; while :; do n=$((n+1)); echo "$n" > /counter.tmp; /bin/busybox mv /counter.tmp /counter; echo "$ROLE:$VALUE:$OVERRIDE:$n"; sleep 1; done`},
			}
		}
		data, err := json.Marshal(map[string]any{"services": map[string]any{
			"worker": service("worker", value), "stable": service("stable", "unchanged"),
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "compose.json.tmp"), data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(project, "compose.json.tmp"), filepath.Join(project, "compose.yaml")); err != nil {
			t.Fatal(err)
		}
	}
	writeCompose("before")
	broadcaster := services.NewTelemetryBroadcaster()
	logs := services.NewContainerLogManager(zap.NewNop(), broadcaster)
	observer := &watchRPCStats{}
	server := grpc.NewServer(grpc.StatsHandler(observer))
	agentpb.RegisterWendyAgentServiceServer(server, services.NewAgentService(zap.NewNop(), nil, nil, nil, nil))
	agentpb.RegisterWendyContainerServiceServer(server, services.NewContainerService(zap.NewNop(), c, services.WithLogManager(logs)))
	agentpb.RegisterWendyTelemetryServiceServer(server, services.NewTelemetryService(zap.NewNop(), broadcaster, nil))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); <-serverDone })
	output := &watchCapturedOutput{}
	cmd := exec.Command(cli, "--device", listener.Addr().String(), "--json=false", "run", "--watch", "--prefix", project,
		"--debounce", "100", "--no-restart", "--skip-cloud-registration", "--env", "OVERRIDE=cli")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "WENDY_CONFIG_DIR=" + filepath.Join(root, "config"),
		"WENDY_SECRET_STORE=file", "CI=1", "WENDY_ANALYTICS=false", "WENDY_NO_UPDATE_CHECK=1", "TERM=dumb"}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// os.Process owns a pidfd on Linux; Wait is called exactly once. The outer
	// PID namespace is the final cleanup boundary if this bounded join fails.
	cliDone := make(chan error, 1)
	go func() { cliDone <- cmd.Wait() }()
	cliJoined := false
	t.Cleanup(func() {
		if cliJoined {
			return
		}
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-cliDone:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			select {
			case <-cliDone:
			case <-time.After(5 * time.Second):
				t.Error("owned watch CLI did not drain")
			}
		}
	})
	wait := func(boundary string, duration time.Duration, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(duration)
		for time.Now().Before(deadline) {
			if condition() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("watch container boundary failed: %s\n%s", boundary, output.String())
	}
	wait("initial actual container output", 40*time.Second, func() bool {
		text := output.String()
		return strings.Contains(text, "worker:before:cli:") && strings.Contains(text, "stable:unchanged:cli:")
	})
	if starts, active, heartbeats := observer.snapshot(); starts != 1 || active != 1 || heartbeats < 1 {
		t.Fatalf("quiet startup lost the actual session stream: starts=%d active=%d heartbeats=%d", starts, active, heartbeats)
	}
	taskFor := func(app string) containerd.Task {
		t.Helper()
		ctr, err := c.client.LoadContainer(ctx, app)
		if err != nil {
			t.Fatal(err)
		}
		task, err := ctr.Task(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	oldWorker, oldStable := taskFor(worker), taskFor(stable)
	oldBirth := watchTaskBirth(oldWorker.Pid())
	stableBirth := watchTaskBirth(oldStable.Pid())
	if oldBirth == "" || stableBirth == "" {
		t.Fatal("missing original worker process birth")
	}
	stableCount := watchContainerCounter(t, ctx, c, stable)
	writeCompose("after")
	wait("changed service real output", 40*time.Second, func() bool {
		return strings.Contains(output.String(), "worker:after:cli:")
	})
	newWorker, newStable := taskFor(worker), taskFor(stable)
	if newWorker.Pid() == oldWorker.Pid() {
		t.Fatal("watched configuration edit did not replace worker task")
	}
	if newStable.Pid() != oldStable.Pid() || watchTaskBirth(newStable.Pid()) != stableBirth {
		t.Fatal("unchanged Compose service was restarted")
	}
	if watchTaskBirth(oldWorker.Pid()) == oldBirth {
		t.Fatal("replaced worker task remains alive")
	}
	wait("preserved sibling live output across replacement", 5*time.Second, func() bool {
		return strings.Contains(output.String(), fmt.Sprintf("stable:unchanged:cli:%d", stableCount+2))
	})
	if starts, active, _ := observer.snapshot(); starts != 1 || active != 1 {
		t.Fatalf("watch reopened its telemetry stream across redeploy: starts=%d active=%d", starts, active)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-cliDone:
		cliJoined = true
		if err != nil {
			t.Fatalf("watch interrupt: %v\n%s", err, output.String())
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("watch interrupt did not exit\n%s", output.String())
	}
	wait("server telemetry producer cancellation", 5*time.Second, func() bool { _, active, _ := observer.snapshot(); return active == 0 })
	for _, app := range []string{worker, stable} {
		before := watchContainerCounter(t, ctx, c, app)
		wait("container remains active after watch interrupt: "+app, 5*time.Second, func() bool {
			return watchContainerCounter(t, ctx, c, app) > before
		})
	}
	t.Log("WATCH_COMPOSE_VERIFIED actual containers, quiet heartbeat, env override, changed output, preserved sibling, interrupt drain, app survival")
}

func watchContainerCounter(t *testing.T, ctx context.Context, c *Client, app string) int {
	t.Helper()
	op, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(op, "ctr", "--address", os.Getenv(containerdIntegrationEnv), "--namespace", c.namespace,
		"tasks", "exec", "--exec-id", "counter-"+strconv.FormatInt(time.Now().UnixNano(), 36), app, "/bin/busybox", "cat", "/counter")
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("live container counter %s: %v: %s", app, err, data)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("invalid live counter %q: %v", data, err)
	}
	return n
}

type watchCapturedOutput struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (o *watchCapturedOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.data.Len()+len(p) > 1<<20 {
		return 0, io.ErrShortBuffer
	}
	return o.data.Write(p)
}
func (o *watchCapturedOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.data.String()
}

type watchRPCKey struct{}
type watchRPCStats struct {
	mu                         sync.Mutex
	starts, active, heartbeats int
}

func (s *watchRPCStats) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	return context.WithValue(ctx, watchRPCKey{}, strings.HasSuffix(info.FullMethodName, "/StreamLogs"))
}
func (s *watchRPCStats) HandleRPC(ctx context.Context, event stats.RPCStats) {
	if ok, _ := ctx.Value(watchRPCKey{}).(bool); !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev := event.(type) {
	case *stats.Begin:
		s.starts++
		s.active++
	case *stats.End:
		s.active--
	case *stats.OutPayload:
		if message, ok := ev.Payload.(*agentpb.StreamLogsResponse); ok && message.Logs == nil {
			s.heartbeats++
		}
	}
}
func (s *watchRPCStats) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
func (s *watchRPCStats) HandleConn(context.Context, stats.ConnStats) {}
func (s *watchRPCStats) snapshot() (int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts, s.active, s.heartbeats
}

// Task.Status addresses the container ID, which is deliberately reused. A
// kernel PID plus birth time distinguishes the old process from its successor.
func watchTaskBirth(pid uint32) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}
