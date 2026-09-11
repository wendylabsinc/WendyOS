package commands

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestNativeCommandDeployment(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "launch.py"), []byte("#!/usr/bin/python3\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Brewfile.wendy"), []byte("brew \"uv\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &appconfig.AppConfig{AppID: "sh.wendy.NativeTest", Platform: "darwin", Run: &appconfig.RunConfig{Command: "launch.py", Cwd: ".", Args: []string{"hello world", "$(literal)"}}, Env: map[string]string{"MODE": "app", "EMPTY": "app"}}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "wendy.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	// The explicit command takes precedence even with a Docker project marker.
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch"), 0644); err != nil {
		t.Fatal(err)
	}
	if project, err := resolveRunProjectType(dir, ""); err != nil || project != "native-process" {
		t.Fatalf("project=%q err=%v", project, err)
	}
	state := &fakeMacRunState{}
	conn, cleanup := startFakeMacRunServer(t, state)
	defer cleanup()
	version := &agentpb.GetAgentVersionResponse{Os: "darwin", Featureset: []string{"native-process"}}
	opts := runOptions{deploy: true, env: []string{"MODE=first", "MODE=cli", "EMPTY="}, noRestart: true}
	if err := runNativeCommandWithAgent(context.Background(), conn, dir, cfg, opts, version); err != nil {
		t.Fatal(err)
	}
	if len(state.createReqs) != 1 {
		t.Fatalf("creates=%d", len(state.createReqs))
	}
	req := state.createReqs[0]
	if req.Cmd != "launch.py" || req.WorkingDir != "." || !reflect.DeepEqual(req.UserArgs, cfg.Run.Args) {
		t.Fatalf("request: %v", req)
	}
	slices.Sort(req.Env)
	if !reflect.DeepEqual(req.Env, []string{"EMPTY=", "MODE=cli"}) {
		t.Fatalf("env=%v", req.Env)
	}
	if req.RestartPolicy.GetMode() != agentpb.RestartPolicyMode_NO {
		t.Fatalf("restart=%v", req.RestartPolicy)
	}
	if cfg.Brewfile != "Brewfile.wendy" {
		t.Fatal("Brewfile not synced")
	}
}

func TestNativeCommandWatchDetectsLaunchAndFileChanges(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "launch.py"), []byte("#!/usr/bin/python3\n"), 0755)
	os.WriteFile(filepath.Join(dir, "wendy.json"), []byte(`{"appId":"sh.wendy.Watch"}`), 0644)
	cfg := &appconfig.AppConfig{AppID: "sh.wendy.Watch", Platform: "darwin", Run: &appconfig.RunConfig{Command: "launch.py"}}
	state := &fakeMacRunState{}
	conn, cleanup := startFakeMacRunServer(t, state)
	defer cleanup()
	version := &agentpb.GetAgentVersionResponse{Os: "darwin", Featureset: []string{"native-process"}}
	opts := runOptions{detach: true, watchState: &watchDeployState{hashes: map[string]string{}}}
	run := func(want int) {
		t.Helper()
		if err := runNativeCommandWithAgent(context.Background(), conn, dir, cfg, opts, version); err != nil {
			t.Fatal(err)
		}
		if len(state.createReqs) != want {
			t.Fatalf("creates=%d want=%d", len(state.createReqs), want)
		}
	}
	run(1)
	run(1)
	opts.env = []string{"MODE=changed"}
	run(2)
	cfg.Run.Cwd = "."
	run(3)
	cfg.Run.Args = []string{"literal arg"}
	run(4)
	opts.noRestart = true
	run(5)
	os.WriteFile(filepath.Join(dir, "launch.py"), []byte("#!/usr/bin/python3\nprint('new')\n"), 0755)
	run(6)
	cfg.Run.Command = "./launch.py"
	run(7)
}

func TestNativeCommandRequiresCapabilityAndRejectsBuildOptions(t *testing.T) {
	cfg := &appconfig.AppConfig{AppID: "sh.wendy.Test", Run: &appconfig.RunConfig{Command: "/usr/bin/python3"}}
	version := &agentpb.GetAgentVersionResponse{Os: "darwin"}
	if err := validateNativeCommandOptions(cfg, runOptions{}, version); err == nil {
		t.Fatal("older agent accepted")
	}
	version.Featureset = []string{"native-process"}
	for _, opts := range []runOptions{{buildType: "docker"}, {dockerfile: "Dockerfile"}, {product: "App"}, {buildHost: "device"}, {debug: true}} {
		if err := validateNativeCommandOptions(cfg, opts, version); err == nil {
			t.Fatalf("accepted %+v", opts)
		}
	}
	if err := validateNativeCommandOptions(cfg, runOptions{}, version); err != nil {
		t.Fatal(err)
	}
}
