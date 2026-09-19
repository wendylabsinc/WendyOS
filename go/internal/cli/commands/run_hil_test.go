package commands

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func hilFixture(t *testing.T) (string, *appconfig.HILConfig) {
	t.Helper()
	root := t.TempDir()
	for path, content := range map[string]string{
		"hil/wendy.json":           `{"appId":"test-inference","platform":"linux"}`,
		"hil/build.stagefile.yaml": "version: 1\n",
		"Dockerfile":               "FROM scratch\n",
		"runtime/policy.py":        "weights = 1\n",
	} {
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root, &appconfig.HILConfig{Project: "hil", Inputs: []string{"runtime"},
		BuildFile: "build.stagefile.yaml", SimulatorBuildFile: "Dockerfile", Port: 8098,
		URLEnv: "POLICY_URL", HealthPath: "/health"}
}

func TestHILStagesSeparateAppAndCleansUp(t *testing.T) {
	root, cfg := hilFixture(t)
	if err := validateHILConfig(root, cfg); err != nil {
		t.Fatal(err)
	}
	dir, cleanup, err := stageHILProject(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, path := range []string{"wendy.json", "build.stagefile.yaml", "runtime/policy.py"} {
		if _, err := os.Stat(filepath.Join(dir, path)); err != nil {
			t.Fatal(err)
		}
	}
	app, err := appconfig.LoadFromFile(filepath.Join(dir, "wendy.json"))
	if err != nil || app.AppID != "test-inference" {
		t.Fatalf("app=%v err=%v", app, err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("staging directory remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "runtime/policy.py")); err != nil {
		t.Fatal(err)
	}
}

func TestHILRejectsPathsAndConfigurationBeforeDeployment(t *testing.T) {
	for name, mutate := range map[string]func(*appconfig.HILConfig){
		"escape":   func(c *appconfig.HILConfig) { c.Project = "../outside" },
		"root":     func(c *appconfig.HILConfig) { c.Project = "." },
		"port":     func(c *appconfig.HILConfig) { c.Port = 65536 },
		"env":      func(c *appconfig.HILConfig) { c.URLEnv = "INVALID=KEY" },
		"override": func(c *appconfig.HILConfig) { c.Env = map[string]string{"POLICY_URL": "wrong"} },
		"health":   func(c *appconfig.HILConfig) { c.HealthPath = "//elsewhere/health" },
		"inputs":   func(c *appconfig.HILConfig) { c.Inputs = []string{"../runtime"} },
		"empty":    func(c *appconfig.HILConfig) { c.Inputs = nil },
		"build":    func(c *appconfig.HILConfig) { c.BuildFile = "missing" },
	} {
		t.Run(name, func(t *testing.T) {
			root, cfg := hilFixture(t)
			mutate(cfg)
			if err := validateHILConfig(root, cfg); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestHILRejectsSymlinkedInputs(t *testing.T) {
	root, cfg := hilFixture(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "runtime", "external")); err != nil {
		t.Skip(err)
	}
	if _, _, err := stageHILProject(root, cfg); err == nil {
		t.Fatal("followed symbolic link")
	}
	leftovers, _ := filepath.Glob(filepath.Join(root, ".wendy-hil-*"))
	if len(leftovers) != 0 {
		t.Fatalf("left staged context: %v", leftovers)
	}
}

func TestHILForwardCarriesHTTPAndClosesActiveConnections(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(append([]byte("echo:"), body...))
	}))
	defer upstream.Close()
	forward, err := startHILForward(context.Background(), func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(upstream.URL, "http://"))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer forward.Close()
	client := &http.Client{Timeout: time.Second}
	response, err := client.Post("http://"+forward.listener.Addr().String(), "application/octet-stream", strings.NewReader("observations"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "echo:observations" {
		t.Fatalf("reply %q", body)
	}
	conn, err := net.Dial("tcp", forward.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	done := make(chan struct{})
	go func() { forward.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("close hung on active connection")
	}
	if _, err := net.DialTimeout("tcp", forward.listener.Addr().String(), 100*time.Millisecond); err == nil {
		t.Fatal("listener remains open")
	}
}

func TestHILForwardCancellationInterruptsPendingDial(t *testing.T) {
	entered := make(chan struct{})
	forward, err := startHILForward(context.Background(), func(ctx context.Context) (net.Conn, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", forward.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	<-entered
	done := make(chan struct{})
	go func() { forward.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pending dial survived cancellation")
	}
}

func TestHILHealthWaitsForReadyAndHonorsCancellation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 2 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{"ready":true}`))
	}))
	defer server.Close()
	if err := waitHILHealth(context.Background(), server.URL, time.Second); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("attempts=%d", calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitHILHealth(ctx, server.URL, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestHILFlagAndIncompatibleRunModes(t *testing.T) {
	cmd := newRunCmd()
	if cmd.Flags().Lookup("hil-device") != nil || cmd.Flags().Lookup("hil") == nil {
		t.Fatal("native flag missing")
	}
	for _, opts := range []runOptions{{detach: true}, {deploy: true}, {service: "web"}} {
		if err := runHILCommand(context.Background(), opts, "jetson"); err == nil {
			t.Fatal("unsupported lifecycle accepted")
		}
	}
}

func TestHILPickerFlagRejectsUnattendedRuns(t *testing.T) {
	previous := isInteractiveTerminalFn
	t.Cleanup(func() { isInteractiveTerminalFn = previous })
	isInteractiveTerminalFn = func() bool { return false }
	cmd := newRunCmd()
	cmd.SetArgs([]string{"--hil"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--hil=DEVICE") {
		t.Fatalf("expected device selection guidance, got %v", err)
	}
	isInteractiveTerminalFn = func() bool { return true }
	if err := runHILCommand(context.Background(), runOptions{yes: true}, ""); err == nil || !strings.Contains(err.Error(), "--hil=DEVICE") {
		t.Fatalf("--yes opened a picker: %v", err)
	}
	cmd = newRunCmd()
	cmd.SetArgs([]string{"--hil=jetson", "--detach"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "attached") {
		t.Fatalf("explicit device did not bypass picker: %v", err)
	}
}

func TestHILSelectsBuildFileForInferenceGPU(t *testing.T) {
	root, cfg := hilFixture(t)
	cfg.BuildFilesByGPUArch = map[string]string{"sm_121": "./spark.stagefile.yaml"}
	cfg.BuildFile = "./build.stagefile.yaml"
	file := filepath.Join(root, "hil", "spark.stagefile.yaml")
	if err := os.WriteFile(file, []byte("version: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateHILConfig(root, cfg); err != nil {
		t.Fatal(err)
	}
	for arch, want := range map[string]string{"sm_121": "spark.stagefile.yaml", "sm_87": "build.stagefile.yaml"} {
		if got := hilBuildFile(cfg, arch); got != want {
			t.Fatalf("%s selects %s, want %s", arch, got, want)
		}
	}
	dir, cleanup, err := stageHILProject(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(dir, "spark.stagefile.yaml")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []map[string]string{
		{"sm_121": "../Dockerfile"}, {"sm_121": "missing.stagefile.yaml"},
		{"sm_121": ""}, {"": "spark.stagefile.yaml"},
	} {
		cfg.BuildFilesByGPUArch = bad
		if err := validateHILConfig(root, cfg); err == nil {
			t.Fatalf("invalid build mapping accepted: %v", bad)
		}
	}
}

func TestHILHealthAuthenticatesAndChecksSchema(t *testing.T) {
	for _, body := range []string{`{"schema":"inference.v1"}`, `{"schema":"other"}`, "not JSON"} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer per-run-secret" {
					t.Error("missing bearer token")
				}
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			err := waitHILHealthAuthenticated(context.Background(), server.URL, time.Second, "per-run-secret", "inference.v1")
			if (err == nil) != strings.Contains(body, "inference.v1") {
				t.Fatalf("body=%s err=%v", body, err)
			}
		})
	}
}

func TestHILTokenEnvironmentCannotBeOverridden(t *testing.T) {
	root, cfg := hilFixture(t)
	cfg.TokenEnv = "POLICY_TOKEN"
	cfg.Env = map[string]string{"POLICY_TOKEN": "fixed"}
	if err := validateHILConfig(root, cfg); err == nil {
		t.Fatal("accepted a fixed token override")
	}
	cfg.Env = nil
	cfg.TokenEnv = cfg.URLEnv
	if err := validateHILConfig(root, cfg); err == nil {
		t.Fatal("accepted overlapping managed variables")
	}
}

func TestBuildHostPickerSkipsSimulatorTab(t *testing.T) {
	m := devicePickerModel{purpose: buildHostPicker, active: devicePickerLocalTab}
	for range 5 {
		m.active = cycleTab(m.tabOrder(), m.active, 1)
		if m.active == devicePickerSimulatorTab {
			t.Fatal("build host offered a simulator")
		}
	}
}

func TestHILPersistsSelectedStagefileLock(t *testing.T) {
	for _, source := range []string{"build.stagefile.yaml", "spark.stagefile.yaml", "./build.stagefile.yaml", "./spark.stagefile.yaml"} {
		stage, project := t.TempDir(), t.TempDir()
		name := strings.TrimSuffix(source, ".yaml") + ".lock.yaml"
		contents := []byte("version: 1\nimages:\n  ubuntu: sha256:pinned\n")
		if err := os.WriteFile(filepath.Join(stage, name), contents, 0600); err != nil {
			t.Fatal(err)
		}
		if err := persistHILStagefileLock(stage, project, source); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(project, name))
		if err != nil || !strings.Contains(string(data), "sha256:pinned") {
			t.Fatalf("lock not preserved: %s %v", data, err)
		}
	}
	if err := persistHILStagefileLock(t.TempDir(), t.TempDir(), "Dockerfile"); err != nil {
		t.Fatal(err)
	}
}

func TestHILHealthRejectsPermanentClientErrorsImmediately(t *testing.T) {
	for _, code := range []int{400, 404, 405} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(code) }))
		err := waitHILHealthAuthenticated(context.Background(), server.URL, time.Second, "", "")
		server.Close()
		if err == nil || calls != 1 {
			t.Fatalf("status %d: calls=%d err=%v", code, calls, err)
		}
	}
}
