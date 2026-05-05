package containerd

import (
	"errors"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestCreateContainerProgressMappingUsesApplyPhase(t *testing.T) {
	progress := UnpackProgress{
		Phase:       "layer",
		LayerIndex:  2,
		TotalLayers: 5,
		LayerSize:   1234,
		Reused:      true,
	}

	got := toCreateContainerProgress(progress)

	if got.GetPhase() != agentpb.CreateContainerProgress_APPLYING_LAYER {
		t.Fatalf("phase = %v; want APPLYING_LAYER", got.GetPhase())
	}
	if got.GetLayerIndex() != 2 {
		t.Fatalf("layer index = %d; want 2", got.GetLayerIndex())
	}
	if got.GetTotalLayers() != 5 {
		t.Fatalf("total layers = %d; want 5", got.GetTotalLayers())
	}
	if got.GetLayerSize() != 1234 {
		t.Fatalf("layer size = %d; want 1234", got.GetLayerSize())
	}
	if !got.GetReusedSnapshot() {
		t.Fatal("expected reused snapshot to be true")
	}
}

func TestCreateContainerProgressMappingUsesUnpackingPhaseForStart(t *testing.T) {
	progress := UnpackProgress{
		Phase:       "start",
		TotalLayers: 3,
	}

	got := toCreateContainerProgress(progress)

	if got.GetPhase() != agentpb.CreateContainerProgress_UNPACKING {
		t.Fatalf("phase = %v; want UNPACKING", got.GetPhase())
	}
	if got.GetTotalLayers() != 3 {
		t.Fatalf("total layers = %d; want 3", got.GetTotalLayers())
	}
	if got.GetLayerIndex() != 0 {
		t.Fatalf("layer index = %d; want 0", got.GetLayerIndex())
	}
}

func TestBuildContainerBaseEnvIncludesDeviceHostname(t *testing.T) {
	old := deviceHostnameWithSuffix
	t.Cleanup(func() { deviceHostnameWithSuffix = old })
	deviceHostnameWithSuffix = func() string { return "wendyos-test-device.local" }

	env := buildContainerBaseEnv("camera-app")

	wantApp := "WENDY_HOSTNAME=camera-app.local"
	wantDevice := "WENDY_DEVICE_HOSTNAME=wendyos-test-device.local"
	var sawApp, sawDevice bool
	for _, kv := range env {
		switch kv {
		case wantApp:
			sawApp = true
		case wantDevice:
			sawDevice = true
		}
	}
	if !sawApp {
		t.Errorf("env missing %q; got %v", wantApp, env)
	}
	if !sawDevice {
		t.Errorf("env missing %q; got %v", wantDevice, env)
	}
}

func TestBuildContainerBaseEnvOmitsDeviceHostnameWhenUnavailable(t *testing.T) {
	old := deviceHostnameWithSuffix
	t.Cleanup(func() { deviceHostnameWithSuffix = old })
	deviceHostnameWithSuffix = func() string { return "" }

	env := buildContainerBaseEnv("camera-app")

	for _, kv := range env {
		if len(kv) >= len("WENDY_DEVICE_HOSTNAME=") && kv[:len("WENDY_DEVICE_HOSTNAME=")] == "WENDY_DEVICE_HOSTNAME=" {
			t.Errorf("env unexpectedly contains %q when device hostname is unresolvable", kv)
		}
	}
}

func TestExpandAgentHook(t *testing.T) {
	t.Setenv("EXTRA_VALUE", "ok")

	got := expandAgentHook("echo ${WENDY_APP_ID} ${WENDY_HOSTNAME} ${EXTRA_VALUE}", "camera-app")
	want := "echo camera-app localhost ok"
	if got != want {
		t.Fatalf("expandAgentHook = %q; want %q", got, want)
	}
}

func TestExpandAgentHookMissingEnv(t *testing.T) {
	t.Setenv("MISSING_VALUE", "")

	got := expandAgentHook("echo ${MISSING_VALUE}", "app")
	if got != "echo " {
		t.Fatalf("expandAgentHook missing env = %q; want empty expansion", got)
	}
}

func TestStartPostStartAgentHookSkippedWhenEmpty(t *testing.T) {
	old := startPostStartHookCommand
	t.Cleanup(func() { startPostStartHookCommand = old })

	var calls int
	startPostStartHookCommand = func(_, _, _ string) (func() error, error) {
		calls++
		return func() error { return nil }, nil
	}

	client := &Client{logger: zap.NewNop()}
	started := client.startPostStartAgentHook("", "camera-app")
	if started {
		t.Fatal("startPostStartAgentHook returned true without command")
	}
	if calls != 0 {
		t.Fatalf("hook runner called %d times; want 0", calls)
	}
}

func TestStartPostStartAgentHookRunsWhenPresent(t *testing.T) {
	t.Setenv("EXTRA_VALUE", "ok")
	old := startPostStartHookCommand
	t.Cleanup(func() { startPostStartHookCommand = old })

	var gotShell, gotFlag, gotCommand string
	startPostStartHookCommand = func(shell, flag, command string) (func() error, error) {
		gotShell = shell
		gotFlag = flag
		gotCommand = command
		return func() error { return nil }, nil
	}

	client := &Client{logger: zap.NewNop()}
	started := client.startPostStartAgentHook("echo ${WENDY_APP_ID} ${WENDY_HOSTNAME} ${EXTRA_VALUE}", "camera-app")
	if !started {
		t.Fatal("startPostStartAgentHook returned false with command")
	}
	if gotShell == "" || gotFlag == "" {
		t.Fatalf("shell command not populated: shell=%q flag=%q", gotShell, gotFlag)
	}
	wantCommand := "echo camera-app localhost ok"
	if gotCommand != wantCommand {
		t.Fatalf("hook command = %q; want %q", gotCommand, wantCommand)
	}
}

func TestStartPostStartAgentHookStartErrorDoesNotLogCommand(t *testing.T) {
	old := startPostStartHookCommand
	t.Cleanup(func() { startPostStartHookCommand = old })

	startPostStartHookCommand = func(_, _, _ string) (func() error, error) {
		return nil, errors.New("start failed")
	}

	core, observed := observer.New(zap.WarnLevel)
	client := &Client{logger: zap.New(core)}
	started := client.startPostStartAgentHook("echo secret-token-value", "camera-app")
	if started {
		t.Fatal("startPostStartAgentHook returned true after start error")
	}

	logs := observed.FilterMessage("Failed to start postStart agent hook")
	if logs.Len() != 1 {
		t.Fatalf("warning log count = %d; want 1", logs.Len())
	}
	if observed.FilterMessageSnippet("secret-token-value").Len() != 0 {
		t.Fatal("hook command leaked into warning message")
	}
	for _, field := range logs.All()[0].Context {
		if field.Key == "command" {
			t.Fatal("hook command leaked into warning fields")
		}
	}
}
