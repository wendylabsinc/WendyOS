package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestDebouncedDeployerUserCancellationStopsWatch(t *testing.T) {
	originalRun := watchRunCommand
	t.Cleanup(func() { watchRunCommand = originalRun })
	watchRunCommand = func(context.Context, runOptions) error { return ErrUserCancelled }

	stopped := make(chan struct{})
	d := &debouncedDeployer{
		stopWatch: func() { close(stopped) },
	}
	d.trigger(context.Background())

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop after the build UI returned ErrUserCancelled")
	}
}

func TestWatchKeepsInitialPickerTargetForRedeploys(t *testing.T) {
	restoreDeviceGlobals(t)
	deviceFlag, jsonOutput = "", false
	t.Setenv("WENDY_AGENT_SOCKET", "")
	oldPicker, oldTTY, oldRun := pickTargetDeviceFn, isInteractiveTerminalFn, watchRunCommand
	t.Cleanup(func() {
		pickTargetDeviceFn, isInteractiveTerminalFn, watchRunCommand = oldPicker, oldTTY, oldRun
	})
	isInteractiveTerminalFn = func() bool { return true }

	picked := 0
	target := &SelectedDevice{External: &models.ExternalDevice{ID: "watch-target"}}
	pickTargetDeviceFn = func(ctx context.Context, _ map[string]bool, _, _, _ bool) (*SelectedDevice, error) {
		picked++
		if devicePickerPurposeFromContext(ctx) != mainDevicePicker {
			t.Error("watch uses a different picker from run")
		}
		return target, nil
	}
	deploys := make(chan runOptions, 2)
	watchRunCommand = func(ctx context.Context, opts runOptions) error {
		select {
		case deploys <- opts:
		case <-ctx.Done():
		}
		return nil
	}

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	finished := make(chan struct{})
	var watchErr error
	go func() {
		defer close(finished)
		watchErr = watchCommand(ctx, runOptions{detach: true, prefix: dir}, 10*time.Millisecond)
	}()
	defer func() {
		cancel()
		<-finished
	}()

	for cycle := range 2 {
		select {
		case opts := <-deploys:
			if opts.watchTarget != target || !opts.yes {
				t.Fatalf("deploy %d did not reuse the selected target with prompts disabled", cycle+1)
			}
		case <-finished:
			t.Fatalf("watch exited before deploy %d: %v", cycle+1, watchErr)
		case <-ctx.Done():
			t.Fatalf("waiting for deploy %d: %v", cycle+1, ctx.Err())
		}
		if cycle == 0 {
			if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("pass\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	cancel()
	<-finished
	if watchErr != nil || picked != 1 || deviceFlag != "" {
		t.Fatalf("watch error=%v picker calls=%d global device=%q", watchErr, picked, deviceFlag)
	}
}

func TestWatchStartupPickerRespectsNonInteractiveModes(t *testing.T) {
	restoreDeviceGlobals(t)
	deviceFlag = ""
	t.Setenv("WENDY_AGENT_SOCKET", "")
	oldPicker, oldTTY := pickTargetDeviceFn, isInteractiveTerminalFn
	t.Cleanup(func() { pickTargetDeviceFn, isInteractiveTerminalFn = oldPicker, oldTTY })
	pickTargetDeviceFn = func(context.Context, map[string]bool, bool, bool, bool) (*SelectedDevice, error) {
		t.Fatal("watch opened a picker with prompts disabled")
		return nil, nil
	}

	for _, tc := range []struct {
		name           string
		tty, json, yes bool
	}{
		{name: "yes", tty: true, yes: true},
		{name: "JSON", tty: true, json: true},
		{name: "no terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jsonOutput = tc.json
			isInteractiveTerminalFn = func() bool { return tc.tty }
			opts := runOptions{yes: tc.yes, prefix: t.TempDir()}
			if err := watchCommand(t.Context(), opts, 0); !errors.Is(err, errNoDevice) {
				t.Fatalf("missing target error = %v, want %v", err, errNoDevice)
			}
		})
	}
}

func TestWatchAliasSupportsDetach(t *testing.T) {
	if flag := newWatchCmd().Flags().Lookup("detach"); flag == nil {
		t.Fatal("wendy watch is missing the --detach flag")
	}
}

func TestWatchSessionUsesPlainProgress(t *testing.T) {
	if watchUsesPlainProgress(context.Background()) {
		t.Fatal("ordinary command context unexpectedly requests plain progress")
	}
	ctx := context.WithValue(context.Background(), watchSessionContextKey{}, struct{}{})
	if !watchUsesPlainProgress(ctx) {
		t.Fatal("watch context must request plain progress while logs share stdout")
	}
}

func TestWatchShouldIgnore(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		rel    string
		ignore bool
	}{
		// Artifacts the deploy pipeline writes into the watched root on every
		// build — reacting to them would cancel each deploy from inside itself.
		{generatedDockerfileName, true},
		{generatedDockerignoreName, true},
		{"build.stagefile.lock.yaml", true},
		{"api/" + generatedDockerfileName, true},
		// ...including the artifacts of every Stagefile variant.
		{"Dockerfile.generated.prod", true},
		{"Dockerfile.generated.prod.dockerignore", true},
		{"prod.stagefile.lock.yaml", true},
		// Real sources must keep triggering redeploys.
		{stagefileSourceName, false},
		{"prod.stagefile.yaml", false},
		{"main.py", false},
		{"Dockerfile", false},
		{"Dockerfile.prod", false},
		// Editor droppings and ignored dirs.
		{"main.py~", true},
		{".git/HEAD", true},
		{"node_modules/pkg/index.js", true},
	}
	for _, c := range cases {
		if got := watchShouldIgnore(filepath.Join(root, filepath.FromSlash(c.rel)), root); got != c.ignore {
			t.Errorf("watchShouldIgnore(%q) = %v, want %v", c.rel, got, c.ignore)
		}
	}
}

func TestSelectPreservedWatchServicesRequiresUnchangedAndRunning(t *testing.T) {
	state := &watchDeployState{hashes: map[string]string{}}
	const deviceKey = "device"
	state.record(watchServiceKey(deviceKey, "app", "api"), "same")
	state.record(watchServiceKey(deviceKey, "app", "worker"), "old")
	state.record(watchServiceKey(deviceKey, "app", "stopped"), "same")

	candidates := map[string]watchServiceCandidate{
		"api":     {appID: "app", containerName: "app_api", desiredHash: "same"},
		"worker":  {appID: "app", containerName: "app_worker", desiredHash: "new"},
		"stopped": {appID: "app", containerName: "app_stopped", desiredHash: "same"},
		"missing": {appID: "app", containerName: "app_missing", desiredHash: "same"},
	}
	deviceStates := map[string]agentpb.AppRunningState{
		"app_api":     agentpb.AppRunningState_RUNNING,
		"app_worker":  agentpb.AppRunningState_RUNNING,
		"app_stopped": agentpb.AppRunningState_STOPPED,
	}

	got := selectPreservedWatchServices(state, deviceKey, candidates, deviceStates)
	if !reflect.DeepEqual(got, map[string]bool{"api": true}) {
		t.Fatalf("preserved services = %v, want only unchanged running api", got)
	}
}

func TestFilterPreservedServicesKeepsChangedDependencyOrder(t *testing.T) {
	ordered := []string{"db", "api", "worker"}
	got := filterPreservedServices(ordered, map[string]bool{"db": true, "worker": true})
	if !reflect.DeepEqual(got, []string{"api"}) {
		t.Fatalf("filtered services = %v, want [api]", got)
	}
	if !reflect.DeepEqual(ordered, []string{"db", "api", "worker"}) {
		t.Fatalf("filter mutated input order: %v", ordered)
	}
}

func TestAdjustSharedNamespacePreserve(t *testing.T) {
	ordered := []string{"primary", "secondary", "worker"}
	preserve := map[string]bool{"secondary": true, "worker": true}
	got := adjustSharedNamespacePreserve(ordered, preserve, "shared-network")
	if len(got) != 0 {
		t.Fatalf("preserved services = %v; primary replacement affects whole shared namespace", got)
	}

	preserve = map[string]bool{"primary": true, "worker": true}
	got = adjustSharedNamespacePreserve(ordered, preserve, "shared-ipc")
	if !reflect.DeepEqual(got, preserve) {
		t.Fatalf("secondary-only change should preserve unrelated group members: got %v, want %v", got, preserve)
	}

	got = adjustSharedNamespacePreserve(ordered, map[string]bool{"secondary": true}, "isolated")
	if !got["secondary"] {
		t.Fatalf("isolated service was unnecessarily marked for restart: %v", got)
	}
}

func TestWatchDesiredHashIncludesRuntimeConfiguration(t *testing.T) {
	a, err := watchDesiredHash(struct{ Env []string }{Env: []string{"MODE=prod"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := watchDesiredHash(struct{ Env []string }{Env: []string{"MODE=dev"}})
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("runtime configuration change did not change desired-state hash")
	}
}
