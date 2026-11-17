//go:build linux

package containerd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	monitor "github.com/wendylabsinc/wendy/go/internal/agent/container"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Real prebuilt-image Compose deployment through CLI/RPC, delayed TCP listeners,
// host hooks, OCI namespaces and the production restart monitor. All host-network
// services live inside the runner's private network namespace, never the host's.
func TestMultiAppSlowReadinessIsolationLifecycle(t *testing.T) {
	if os.Getenv("WENDY_WATCH_CONTAINER_NAMESPACE") != "1" {
		t.Skip("requires private hosted container runner")
	}
	for _, kind := range []string{"mnt", "net", "pid"} {
		current, err := os.Readlink("/proc/self/ns/" + kind)
		parent := os.Getenv("WENDY_WATCH_PARENT_" + kind)
		if err != nil || parent == "" || current == parent {
			t.Fatalf("%s namespace is not private", kind)
		}
	}
	work, cli := os.Getenv("WENDY_WATCH_WORK"), os.Getenv("WENDY_WATCH_CLI")
	if !filepath.IsAbs(cli) || !filepath.IsAbs(work) || !strings.HasPrefix(filepath.Base(work), "wendy-watch-container.") {
		t.Fatal("require owned runner work and exact CLI")
	}
	root, err := os.MkdirTemp(work, "multiapp-")
	if err != nil {
		t.Fatal(err)
	}
	c, ctx := newIntegrationClient(t)
	image := createReplaceTestImage(t, c, ctx)
	imageName := "docker.io/wendy-test/multiapp@" + image.Target().Digest.String()
	if _, err := c.client.ImageService().Create(ctx, images.Image{Name: imageName, Target: image.Target()}); err != nil {
		t.Fatal(err)
	}
	appID := "multiapp-" + c.namespace
	project := filepath.Join(root, appID)
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	names := []string{"alpha", "beta"}
	ports := []int{18651, 18652}
	for _, name := range names {
		app := appID + "_" + name
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if err := c.DeleteContainer(cleanup, app, false); err != nil {
				t.Errorf("cleanup %s: %v", app, err)
			}
		})
	}
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
	probeBody := "namespace-probe-" + appID
	probeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, probeBody) }))
	t.Cleanup(probeServer.Close)
	probeFrom := func(name string, allowed bool) {
		t.Helper()
		op, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		cmd := exec.CommandContext(op, "ctr", "--address", os.Getenv(containerdIntegrationEnv), "--namespace", c.namespace,
			"tasks", "exec", "--exec-id", fmt.Sprintf("probe-%d", time.Now().UnixNano()), appID+"_"+name,
			"/bin/busybox", "wget", "-q", "-T", "2", "-O", "-", probeServer.URL)
		data, err := cmd.CombinedOutput()
		if allowed {
			if err != nil || string(data) != probeBody {
				t.Fatalf("initial host-network probe failed: %s %v %q", name, err, data)
			}
		} else if err == nil || op.Err() != nil || !(strings.Contains(string(data), "Connection refused") || strings.Contains(string(data), "Network is unreachable")) {
			t.Fatalf("private network did not refuse live fixture listener: %s %v %q", name, err, data)
		}
	}
	hookPath := func(name string) string { return filepath.Join(root, name+".hook") }
	writeConfig := func(private bool) {
		t.Helper()
		serviceConfigs := map[string]any{}
		for index, name := range names {
			mode := "host"
			command := `sleep 20; /bin/busybox mkdir /www; echo "$ROLE:$OVERRIDE:<$EMPTY>:$LITERAL" > /www/index.html; echo "LIVE:$ROLE:$OVERRIDE:<$EMPTY>:$LITERAL"; exec /bin/busybox httpd -f -p "127.0.0.1:$PORT" -h /www`
			svc := map[string]any{"image": imageName, "environment": map[string]string{"ROLE": name, "PORT": fmt.Sprint(ports[index]), "OVERRIDE": "compose-" + name, "EMPTY": "not-empty", "LITERAL": "not-literal"}}
			if private {
				mode = "none"
				command = `echo "PRIVATE:$ROLE:$OVERRIDE:<$EMPTY>:$LITERAL"; while :; do sleep 1; done`
			} else {
				svc["x-wendy"] = map[string]any{"readiness": map[string]any{"tcpSocket": map[string]any{"port": ports[index]}, "timeoutSeconds": 3}, "hooks": map[string]any{"postStart": map[string]any{"cli": fmt.Sprintf("printf 'ready-%s' > '%s'", name, hookPath(name))}}}
			}
			svc["network_mode"], svc["command"] = mode, []string{"/bin/sh", "-c", command}
			serviceConfigs[name] = svc
		}
		companion := map[string]any{"appId": appID, "version": "1"}
		if !private {
			companion["isolation"] = "shared-network"
		}
		for file, value := range map[string]any{"compose.yaml": map[string]any{"services": serviceConfigs}, "wendy.json": companion} {
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, file), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	startCLI := func() (*watchCapturedOutput, func()) {
		t.Helper()
		output := &watchCapturedOutput{}
		cmd := exec.Command(cli, "--device", listener.Addr().String(), "--json=false", "run", "--watch", "--prefix", project, "--no-restart", "--skip-cloud-registration", "--env", "OVERRIDE=first", "--env", "OVERRIDE=final", "--env", "EMPTY=", "--env", "LITERAL=${DO_NOT_EXPAND}")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "WENDY_CONFIG_DIR=" + filepath.Join(root, "config"), "WENDY_SECRET_STORE=file", "CI=1", "WENDY_ANALYTICS=false", "WENDY_NO_UPDATE_CHECK=1", "TERM=dumb"}
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		joined := false
		stop := func() {
			t.Helper()
			if joined {
				return
			}
			_ = cmd.Process.Signal(os.Interrupt)
			select {
			case err := <-done:
				joined = true
				if err != nil {
					t.Errorf("multiapp CLI interrupt: %v\n%s", err, output.String())
				}
			case <-time.After(8 * time.Second):
				_ = cmd.Process.Kill()
				select {
				case <-done:
					joined = true
				case <-time.After(5 * time.Second):
				}
				t.Error("multiapp CLI did not drain gracefully")
			}
		}
		t.Cleanup(stop)
		return output, stop
	}
	wait := func(boundary string, duration time.Duration, output *watchCapturedOutput, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(duration)
		for time.Now().Before(deadline) {
			if condition() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("multiapp boundary failed: %s\n%s", boundary, output.String())
	}
	taskFor := func(name string) containerd.Task {
		t.Helper()
		ctr, err := c.client.LoadContainer(ctx, appID+"_"+name)
		if err != nil {
			t.Fatal(err)
		}
		task, err := ctr.Task(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	// No proxies or redirects can make an unrelated endpoint pass readiness.
	transport := &http.Transport{Proxy: nil}
	client := &http.Client{Transport: transport, Timeout: 300 * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer transport.CloseIdleConnections()
	fetch := func(port int) (string, bool) {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			return "", false
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1025))
		return strings.TrimSpace(string(data)), err == nil && resp.StatusCode == 200 && len(data) <= 1024
	}
	writeConfig(false)
	output, stop := startCLI()
	observedQuiet := map[string]bool{}
	wait("each delayed service becomes ready and runs its own hook", 40*time.Second, output, func() bool {
		all := true
		for index, name := range names {
			body, ready := fetch(ports[index])
			hook, hookErr := os.ReadFile(hookPath(name))
			if !ready {
				observedQuiet[name] = true
				if hookErr == nil {
					t.Fatalf("service hook ran before actual readiness: %s", name)
				}
				all = false
				continue
			}
			expected := name + ":final:<>:${DO_NOT_EXPAND}"
			if body != expected {
				t.Fatalf("service effective environment mismatch: %s got %q want %q", name, body, expected)
			}
			if hookErr != nil {
				all = false
				continue
			}
			if string(hook) != "ready-"+name {
				t.Fatalf("wrong service hook %q", hook)
			}
		}
		return all
	})
	for _, name := range names {
		if !observedQuiet[name] || !strings.Contains(output.String(), appID+"_"+name+" is still starting") {
			t.Fatalf("service did not exercise post-timeout readiness observation: %s\n%s", name, output.String())
		}
	}
	if starts, active, heartbeats := observer.snapshot(); starts != 1 || active != 1 || heartbeats < 1 {
		t.Fatalf("quiet service stream lost heartbeat: starts=%d active=%d heartbeats=%d", starts, active, heartbeats)
	}
	hostNS, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		ns, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", taskFor(name).Pid()))
		if err != nil || ns != hostNS {
			t.Fatalf("initial service did not use fixture host namespace: %s %v", ns, err)
		}
	}
	for _, name := range names {
		probeFrom(name, true)
	}
	stop()
	wait("first telemetry stream drained", 5*time.Second, output, func() bool { _, active, _ := observer.snapshot(); return active == 0 })
	// The later full Compose redeploy replaces both siblings before hydration.
	// Exercise the original old-sibling hydration window separately.
	// Replace one real sibling through the production RPC while beta still has
	// its old shared-network labels and running process, then crash the new alpha.
	func() {
		// Recover service membership from actual persisted Compose labels.
		c.RebuildAppStateCaches(ctx)
		if group, grouped := c.GroupRestartAppID(ctx, appID+"_alpha"); !grouped || group != appID {
			t.Fatal("rolling isolation precondition: initial shared group was not registered")
		}
		betaBefore := taskFor("beta")
		betaBeforeBirth := watchTaskBirth(betaBefore.Pid())
		betaContainer, err := c.client.LoadContainer(ctx, appID+"_beta")
		if err != nil {
			t.Fatal(err)
		}
		betaLabels, err := betaContainer.Labels(ctx)
		if err != nil || betaLabels[labelKeyIsolation] != "shared-network" || betaBeforeBirth == "" {
			t.Fatal("rolling isolation precondition: live beta lacks old persisted isolation")
		}
		cfg := &appconfig.AppConfig{AppID: appID, ServiceName: "alpha", Version: "2", Services: map[string]*appconfig.ServiceConfig{"alpha": {}, "beta": {}}}
		cfgBytes, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		rpc, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		defer rpc.Close()
		request := &agentpb.CreateContainerRequest{AppName: appID, ImageName: imageName, AppConfig: cfgBytes, Cmd: "/bin/sh", UserArgs: []string{"-c", "echo ROLLING_PRIVATE_ALPHA; while :; do sleep 1; done"}}
		if _, err := agentpb.NewWendyContainerServiceClient(rpc).CreateContainer(ctx, request); err != nil {
			t.Fatalf("rolling private create RPC: %v", err)
		}
		rollingOutput, err := c.StartContainer(ctx, appID+"_alpha", "", nil)
		if err != nil {
			t.Fatalf("rolling private task start: %v", err)
		}
		captured := &watchCapturedOutput{}
		outputDone := make(chan struct{})
		go func() {
			defer close(outputDone)
			for item := range rollingOutput {
				_, _ = captured.Write(item.Stdout)
				_, _ = captured.Write(item.Stderr)
			}
		}()
		wait("rolling private process emits actual output", 5*time.Second, captured, func() bool { return strings.Contains(captured.String(), "ROLLING_PRIVATE_ALPHA") })
		alpha := taskFor("alpha")
		alphaBirth := watchTaskBirth(alpha.Pid())
		if ns, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", alpha.Pid())); err != nil || ns == hostNS || alphaBirth == "" {
			t.Fatalf("rolling private task did not get a real private namespace: %s %v", ns, err)
		}
		probeFrom("alpha", false)
		probeFrom("beta", true)
		// Read actual persisted sibling labels during the replacement window.
		// An authoritative empty isolation must survive this production hydration.
		if _, err := c.ListBootContainers(ctx); err != nil {
			t.Fatal(err)
		}
		rollingMonitor := monitor.NewContainerMonitor(zap.NewNop(), c, logs, 100*time.Millisecond)
		for _, name := range names {
			rollingMonitor.Register(appID+"_"+name, monitor.RestartUnlessStopped, 0)
		}
		rollingCtx, cancelRolling := context.WithCancel(ctx)
		rollingDone := make(chan struct{})
		go func() { defer close(rollingDone); rollingMonitor.Run(rollingCtx) }()
		defer func() {
			cancelRolling()
			select {
			case <-rollingDone:
			case <-time.After(10 * time.Second):
				t.Error("rolling monitor did not drain")
			}
		}()
		if err := alpha.Kill(ctx, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		wait("rolling private service recovers without old sibling restart", 15*time.Second, captured, func() bool {
			betaContainer, betaErr := c.client.LoadContainer(ctx, appID+"_beta")
			if betaErr != nil {
				t.Fatal("removed isolation restarted the healthy sibling during rolling replacement: container lost")
			}
			beta, betaErr := betaContainer.Task(ctx, nil)
			if betaErr != nil || beta.Pid() != betaBefore.Pid() || watchTaskBirth(beta.Pid()) != betaBeforeBirth {
				t.Fatal("removed isolation restarted the healthy sibling during rolling replacement")
			}
			ctr, err := c.client.LoadContainer(ctx, appID+"_alpha")
			if err != nil {
				return false
			}
			task, err := ctr.Task(ctx, nil)
			return err == nil && watchTaskBirth(task.Pid()) != "" && watchTaskBirth(task.Pid()) != alphaBirth
		})
		select {
		case <-outputDone:
		case <-time.After(3 * time.Second):
			t.Fatal("rolling old output pump did not drain")
		}
		t.Log("ROLLING_ISOLATION_VERIFIED old live sibling labels, actual private replacement, independent monitor recovery")
	}()
	// Complete the actual CLI Compose redeploy and retain the existing two-private
	// namespace, environment and healthy-sibling assertions as a separate phase.
	writeConfig(true)
	output, stop = startCLI()
	wait("both private replacement processes report effective environment", 25*time.Second, output, func() bool {
		for _, name := range names {
			if !strings.Contains(output.String(), "PRIVATE:"+name+":final:<>:${DO_NOT_EXPAND}") {
				return false
			}
		}
		return true
	})
	namespaces := map[string]bool{}
	for _, name := range names {
		ns, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", taskFor(name).Pid()))
		if err != nil || ns == hostNS || namespaces[ns] {
			t.Fatalf("isolation removal did not produce independent private namespaces: %s %v", ns, err)
		}
		namespaces[ns] = true
	}
	for _, name := range names {
		probeFrom(name, false)
	}
	// The production monitor must restart only the crashed service. Retaining
	// shared-network in the cache would select RestartGroup and kill its sibling.
	c.RebuildAppStateCaches(ctx)
	if _, err := c.ListBootContainers(ctx); err != nil {
		t.Fatal(err)
	}
	oldAlpha, oldBeta := taskFor("alpha"), taskFor("beta")
	alphaBirth, betaBirth := watchTaskBirth(oldAlpha.Pid()), watchTaskBirth(oldBeta.Pid())
	if alphaBirth == "" || betaBirth == "" {
		t.Fatal("missing task birth evidence")
	}
	mon := monitor.NewContainerMonitor(zap.NewNop(), c, logs, 100*time.Millisecond)
	for _, name := range names {
		mon.Register(appID+"_"+name, monitor.RestartUnlessStopped, 0)
	}
	monitorCtx, cancelMonitor := context.WithCancel(ctx)
	monitorDone := make(chan struct{})
	go func() { defer close(monitorDone); mon.Run(monitorCtx) }()
	t.Cleanup(func() {
		cancelMonitor()
		select {
		case <-monitorDone:
		case <-time.After(10 * time.Second):
			t.Error("multiapp monitor did not drain")
		}
	})
	if err := oldAlpha.Kill(ctx, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	wait("crashed private service recovers without sibling restart", 15*time.Second, output, func() bool {
		betaCtr, betaErr := c.client.LoadContainer(ctx, appID+"_beta")
		if betaErr != nil {
			t.Fatal("removed isolation lost the healthy sibling")
		}
		beta, betaErr := betaCtr.Task(ctx, nil)
		if betaErr != nil || beta.Pid() != oldBeta.Pid() || watchTaskBirth(beta.Pid()) != betaBirth {
			t.Fatal("removed isolation restarted the healthy sibling")
		}
		ctr, err := c.client.LoadContainer(ctx, appID+"_alpha")
		if err != nil {
			return false
		}
		task, err := ctr.Task(ctx, nil)
		return err == nil && watchTaskBirth(task.Pid()) != "" && watchTaskBirth(task.Pid()) != alphaBirth
	})
	cancelMonitor()
	select {
	case <-monitorDone:
	case <-time.After(10 * time.Second):
		t.Fatal("multiapp monitor did not drain")
	}
	stop()
	wait("second telemetry stream drained", 5*time.Second, output, func() bool { _, active, _ := observer.snapshot(); return active == 0 })
	t.Log("MULTIAPP_LIFECYCLE_VERIFIED delayed real listeners, per-service hook gating, quiet heartbeat, literal and empty env overrides, private namespace replacement and independent crash recovery")
}
