package commands

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func TestBuildDashboardRows(t *testing.T) {
	running := agentpb.AppRunningState_RUNNING
	stopped := agentpb.AppRunningState_STOPPED

	containers := []*agentpb.AppContainer{
		{AppName: "my-app", AppVersion: "1.0", RunningState: running, FailureCount: 0},
		{AppName: "idle-app", AppVersion: "2.0", RunningState: stopped, FailureCount: 3},
	}

	stats := []*agentpb.ContainerStats{
		{AppName: "my-app", MemoryBytes: 42_000_000, StorageBytes: 128_000_000},
	}

	volumes := []*agentpb.VolumeInfo{
		{Name: "my-app-data", SizeBytes: 64_000_000, UsedBy: []string{"my-app"}},
		{Name: "my-app-cache", SizeBytes: 10_000_000, UsedBy: []string{"my-app"}},
		{Name: "shared", SizeBytes: 5_000_000, UsedBy: []string{"idle-app"}},
	}

	rows := buildDashboardRows(containers, stats, volumes)

	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}

	r0 := rows[0]
	if r0.name != "my-app" {
		t.Errorf("rows[0].name = %q, want %q", r0.name, "my-app")
	}
	if r0.state != "RUNNING" {
		t.Errorf("rows[0].state = %q, want %q", r0.state, "RUNNING")
	}
	if !r0.hasStats {
		t.Error("rows[0].hasStats should be true")
	}
	if r0.memoryBytes != 42_000_000 {
		t.Errorf("rows[0].memoryBytes = %d, want %d", r0.memoryBytes, 42_000_000)
	}
	if r0.storageBytes != 128_000_000 {
		t.Errorf("rows[0].storageBytes = %d, want %d", r0.storageBytes, 128_000_000)
	}
	if r0.volumeCount != 2 {
		t.Errorf("rows[0].volumeCount = %d, want 2", r0.volumeCount)
	}
	if r0.volumeBytes != 74_000_000 {
		t.Errorf("rows[0].volumeBytes = %d, want 74000000", r0.volumeBytes)
	}
	if !r0.hasVolumes {
		t.Error("rows[0].hasVolumes should be true")
	}

	r1 := rows[1]
	if r1.name != "idle-app" {
		t.Errorf("rows[1].name = %q, want %q", r1.name, "idle-app")
	}
	if r1.hasStats {
		t.Error("rows[1].hasStats should be false (no stats entry)")
	}
	if r1.failures != 3 {
		t.Errorf("rows[1].failures = %d, want 3", r1.failures)
	}
	if r1.volumeCount != 1 {
		t.Errorf("rows[1].volumeCount = %d, want 1", r1.volumeCount)
	}
	if !r1.hasVolumes {
		t.Error("rows[1].hasVolumes should be true")
	}
}

func TestAppsDashboardModel_ConfirmFlow(t *testing.T) {
	m := newAppsDashboardModel(nil, context.Background())
	// Seed one row so cursor is valid.
	m.cachedContainers = []*agentpb.AppContainer{
		{AppName: "test-app", RunningState: agentpb.AppRunningState_RUNNING},
	}
	m.refreshTable()

	// Press 'r' — should enter confirming state.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = updated.(appsDashboardModel)
	if !m.confirming {
		t.Fatal("expected confirming=true after pressing r")
	}
	if m.confirmText == "" {
		t.Fatal("expected confirmText to be set")
	}

	// Press 'n' — should cancel.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = updated.(appsDashboardModel)
	if m.confirming {
		t.Fatal("expected confirming=false after pressing n")
	}
}

func TestAppsDashboardModel_QuitAction(t *testing.T) {
	m := newAppsDashboardModel(nil, context.Background())

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	m = updated.(appsDashboardModel)
	if cmd == nil {
		t.Fatal("expected quit command after q")
	}
	_ = m
}

func TestAppsDashboardModel_EnterSetsActionLogs(t *testing.T) {
	m := newAppsDashboardModel(nil, context.Background())
	m.cachedContainers = []*agentpb.AppContainer{
		{AppName: "my-app", RunningState: agentpb.AppRunningState_RUNNING},
	}
	m.refreshTable()

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(appsDashboardModel)
	if cmd == nil {
		t.Fatal("expected quit command after enter")
	}
	if m.action != appsDashActionLogs {
		t.Fatalf("action = %d, want appsDashActionLogs", m.action)
	}
	if m.selectedApp != "my-app" {
		t.Fatalf("selectedApp = %q, want %q", m.selectedApp, "my-app")
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{999, "999 B"},
		{1000, "1.0 kB"},
		{1500, "1.5 kB"},
		{999_999, "1000.0 kB"},
		{1_000_000, "1.0 MB"},
		{42_000_000, "42.0 MB"},
		{1_000_000_000, "1.0 GB"},
		{1_500_000_000, "1.5 GB"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatBytes(tt.n)
			if got != tt.want {
				t.Fatalf("formatBytes(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}
