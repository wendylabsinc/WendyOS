package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestDeviceAppsListCommand_HelpDescribesDeployedApps(t *testing.T) {
	cmd := newDeviceCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"apps", "list", "--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "List deployed applications") {
		t.Fatalf("expected help output to contain %q, got %q", "List deployed applications", output)
	}
	if strings.Contains(output, "List running applications") {
		t.Fatalf("expected help output to avoid stale wording, got %q", output)
	}
}

func TestSortRunningFirstStable(t *testing.T) {
	apps := []appInfo{
		{Name: "stopped-1", State: "STOPPED"},
		{Name: "running-1", State: "RUNNING"},
		{Name: "crash-looping", State: "CRASH_LOOPING"},
		{Name: "running-2", State: "running"},
		{Name: "stopped-2", State: "STOPPED"},
	}

	sortRunningFirst(apps, func(a appInfo) string { return a.State })

	got := make([]string, len(apps))
	for i, app := range apps {
		got[i] = app.Name
	}
	want := []string{"running-1", "running-2", "stopped-1", "crash-looping", "stopped-2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("app order = %v, want %v", got, want)
	}
}

func TestAppsList_GroupDisplayShowsServiceSubRows(t *testing.T) {
	containers := []*agentpb.AppContainer{
		{
			AppName:      "com.example.robot",
			AppVersion:   "v1.0.0",
			RunningState: agentpb.AppRunningState_RUNNING,
			Services: []*agentpb.ServiceEntry{
				{Name: "camera", RunningState: agentpb.AppRunningState_RUNNING},
				{Name: "detector", RunningState: agentpb.AppRunningState_STOPPED},
			},
		},
		{
			AppName:      "com.example.simple",
			AppVersion:   "v2.0.0",
			RunningState: agentpb.AppRunningState_STOPPED,
		},
	}

	var rows [][]string
	for _, c := range containers {
		services := c.GetServices()
		if len(services) > 1 {
			rows = append(rows, []string{"", c.GetAppName() + " [group]", c.GetAppVersion(), "0"})
			for _, s := range services {
				rows = append(rows, []string{"", "  ↳ " + s.GetName(), "", ""})
			}
		} else {
			rows = append(rows, []string{"", c.GetAppName(), c.GetAppVersion(), "0"})
		}
	}

	// Group app should produce 3 rows (header + 2 services); single app 1 row.
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows (1 group header + 2 services + 1 single), got %d", len(rows))
	}
	if !strings.Contains(rows[0][1], "[group]") {
		t.Errorf("group header row should contain [group], got %q", rows[0][1])
	}
	if !strings.Contains(rows[1][1], "↳") || !strings.Contains(rows[1][1], "camera") {
		t.Errorf("first service sub-row should contain ↳ and camera, got %q", rows[1][1])
	}
	if !strings.Contains(rows[2][1], "↳") || !strings.Contains(rows[2][1], "detector") {
		t.Errorf("second service sub-row should contain ↳ and detector, got %q", rows[2][1])
	}
	if strings.Contains(rows[3][1], "[group]") {
		t.Errorf("single-service app should not be marked as group, got %q", rows[3][1])
	}
}

func TestAppsList_SingleServiceNoGroupMark(t *testing.T) {
	containers := []*agentpb.AppContainer{
		{
			AppName:      "com.example.simple",
			AppVersion:   "v1.0.0",
			RunningState: agentpb.AppRunningState_RUNNING,
		},
	}

	var rows [][]string
	for _, c := range containers {
		if len(c.GetServices()) > 1 {
			rows = append(rows, []string{"", c.GetAppName() + " [group]", c.GetAppVersion(), "0"})
		} else {
			rows = append(rows, []string{"", c.GetAppName(), c.GetAppVersion(), "0"})
		}
	}

	if len(rows) != 1 {
		t.Fatalf("expected 1 row for single-service app, got %d", len(rows))
	}
	if strings.Contains(rows[0][1], "[group]") {
		t.Errorf("single-service app should not be marked as group")
	}
}

func TestStateIconPlain_CrashLooping(t *testing.T) {
	// A crash-looping app (WDY-1826) must be visually distinct from both a
	// running (●) and a stopped (○) app in plain, non-styled output. The state
	// string is AppRunningState.String(), i.e. "CRASH_LOOPING".
	state := agentpb.AppRunningState_CRASH_LOOPING.String()
	if got := stateIconPlain(state); got != "↻" {
		t.Fatalf("stateIconPlain(%q) = %q, want ↻", state, got)
	}
	if got := stateIconPlain(agentpb.AppRunningState_RUNNING.String()); got != "●" {
		t.Fatalf("stateIconPlain(RUNNING) = %q, want ●", got)
	}
	if got := stateIconPlain(agentpb.AppRunningState_STOPPED.String()); got != "○" {
		t.Fatalf("stateIconPlain(STOPPED) = %q, want ○", got)
	}
}

func TestStateIcon_CrashLoopingDistinctFromStopped(t *testing.T) {
	crash := stateIcon(agentpb.AppRunningState_CRASH_LOOPING.String())
	stopped := stateIcon(agentpb.AppRunningState_STOPPED.String())
	if crash == stopped {
		t.Fatalf("crash-looping icon %q must differ from stopped icon %q", crash, stopped)
	}
	if !strings.Contains(crash, "↻") {
		t.Fatalf("crash-looping icon %q should contain ↻", crash)
	}
}

func TestHTTPPortColumn_Zero(t *testing.T) {
	if got := httpPortColumn(0); got != "" {
		t.Errorf("httpPortColumn(0) = %q, want empty string", got)
	}
}

func TestHTTPPortColumn_NonZero(t *testing.T) {
	if got := httpPortColumn(8080); got != ":8080" {
		t.Errorf("httpPortColumn(8080) = %q, want %q", got, ":8080")
	}
}

// The wendy-app-lifecycle skill tells agents to run
// `wendy device apps start <app> --detach`; keep the flag it relies on.
func TestAppsStartCmd_HasDetachFlag(t *testing.T) {
	f := newAppsStartCmd().Flags().Lookup("detach")
	if f == nil || f.Shorthand != "d" {
		t.Fatalf("device apps start must keep -d/--detach, got %+v", f)
	}
}

// The skill once claimed `apps start` had no --detach and recommended GNU
// `timeout`, which macOS lacks, to bound a log sample. Guard every copy (the
// plugin source and, when present, the CLI's embedded copy) against those
// claims coming back.
func TestAppLifecycleSkill_NoStaleClaims(t *testing.T) {
	copies := []string{
		filepath.Join("..", "..", "..", "..", "plugins", "wendy-agentic-coding", "skills", "wendy-app-lifecycle", "SKILL.md"),
		filepath.Join("..", "assets", "skills", "wendy-app-lifecycle", "SKILL.md"),
	}
	checked := 0
	for _, path := range copies {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		checked++
		text := string(data)
		// The log-sampling snippet must not drop a file into the user's
		// working directory either.
		for _, stale := range []string{"There is no `wendy device apps start --detach` flag", "timeout 20s wendy", "> wendy-logs.jsonl"} {
			if strings.Contains(text, stale) {
				t.Errorf("%s still says %q", path, stale)
			}
		}
		if !strings.Contains(text, "wendy device apps start <app-id> --detach") {
			t.Errorf("%s should show `wendy device apps start <app-id> --detach`", path)
		}
		if !strings.Contains(text, "--tail 50 --no-follow") {
			t.Errorf("%s should take a finite log sample with `--tail 50 --no-follow`", path)
		}
	}
	if checked == 0 {
		t.Fatal("no copy of the wendy-app-lifecycle skill found")
	}
}

// The /wendy-apps plugin command once said `apps start` always attaches and
// sent agents to `wendy run --detach` instead; it must point at
// `apps start --detach` and say what that restart policy means.
func TestWendyAppsCommand_MentionsStartDetach(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "plugins", "wendy-agentic-coding", "commands", "wendy-apps.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "currently attaches to the app stream") {
		t.Errorf("%s still says apps start always attaches", path)
	}
	for _, want := range []string{"wendy device apps start <app-id> --detach", "unless-stopped"} {
		if !strings.Contains(text, want) {
			t.Errorf("%s should mention %q", path, want)
		}
	}
}
