package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

type topFakeContainerClient struct {
	agentpb.WendyContainerServiceClient
	stopCalls []string
	stopErr   error
}

func (f *topFakeContainerClient) StopContainer(_ context.Context, req *agentpb.StopContainerRequest, _ ...grpc.CallOption) (*agentpb.StopContainerResponse, error) {
	f.stopCalls = append(f.stopCalls, req.GetAppName())
	if f.stopErr != nil {
		return nil, f.stopErr
	}
	return &agentpb.StopContainerResponse{}, nil
}

func approx(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestFormatThermalZones(t *testing.T) {
	zones := []*agentpb.ThermalZone{
		{Name: "gpu-thermal", TempC: 52.4},
		{Name: "cpu-thermal", TempC: 49},
		{Name: "soc0-therm", TempC: 47},
		{Name: "thermal_zone9", TempC: 40},
	}
	got := formatThermalZones(zones)
	want := "gpu 52°C  cpu 49°C  soc0 47°C  thermal_zone9 40°C"
	if got != want {
		t.Errorf("formatThermalZones = %q; want %q", got, want)
	}
	if formatThermalZones(nil) != "" {
		t.Errorf("formatThermalZones(nil) should be empty")
	}
}

func TestStabilizeThermalZoneOrderUsesHysteresis(t *testing.T) {
	previous := []*agentpb.ThermalZone{
		{Name: "go2/motor/fr-hip", TempC: 52},
		{Name: "go2/motor/fl-hip", TempC: 51},
		{Name: "go2/motor/rr-hip", TempC: 45},
	}

	// The agent now reports fl-hip first, but its one-degree lead is visually
	// insignificant and should not make the dashboard rows trade places.
	current := []*agentpb.ThermalZone{
		{Name: "go2/motor/fl-hip", TempC: 52},
		{Name: "go2/motor/fr-hip", TempC: 51},
		{Name: "go2/motor/rr-hip", TempC: 45},
	}
	stable := stabilizeThermalZoneOrder(previous, current)
	if got := thermalZoneNames(stable); got != "go2/motor/fr-hip,go2/motor/fl-hip,go2/motor/rr-hip" {
		t.Fatalf("near-equal readings reordered: %s", got)
	}

	// A clear lead still promotes the genuinely hotter sensor.
	current[0].TempC = 54
	stable = stabilizeThermalZoneOrder(stable, current)
	if got := thermalZoneNames(stable); got != "go2/motor/fl-hip,go2/motor/fr-hip,go2/motor/rr-hip" {
		t.Fatalf("meaningfully hotter sensor was not promoted: %s", got)
	}

	// Crossing back by only one degree does not immediately undo that move.
	current[0].TempC = 52
	current[1].TempC = 53
	stable = stabilizeThermalZoneOrder(stable, current)
	if got := thermalZoneNames(stable); got != "go2/motor/fl-hip,go2/motor/fr-hip,go2/motor/rr-hip" {
		t.Fatalf("order oscillated after a small reversal: %s", got)
	}
}

func TestStabilizeThermalZoneOrderPreservesDuplicateNames(t *testing.T) {
	current := []*agentpb.ThermalZone{
		{Name: "x86_pkg_temp", TempC: 50},
		{Name: "x86_pkg_temp", TempC: 49},
	}
	if got := stabilizeThermalZoneOrder(nil, current); len(got) != len(current) {
		t.Fatalf("duplicate-named zones were dropped: got %d, want %d", len(got), len(current))
	}
}

func thermalZoneNames(zones []*agentpb.ThermalZone) string {
	names := make([]string, len(zones))
	for i, zone := range zones {
		names[i] = zone.GetName()
	}
	return strings.Join(names, ",")
}

func TestSummarizeTemperatureUsesSensorSpecificThreshold(t *testing.T) {
	host := &agentpb.HostStats{ThermalZones: []*agentpb.ThermalZone{
		{Name: "go2/imu", TempC: 79},
		{Name: "go2/motor/fr-thigh", TempC: 66},
		{Name: "cpu-thermal", TempC: 55},
	}}
	summary, ok := summarizeTemperature(host)
	if !ok {
		t.Fatal("expected a temperature summary")
	}
	if summary.Max.Name != "go2/imu" || summary.Max.TempC != 79 {
		t.Fatalf("max = %+v, want 79C Go2 IMU", summary.Max)
	}
	if summary.Risk != thermalNear {
		t.Fatalf("risk = %v, want near because a motor is within 5C of 70C", summary.Risk)
	}
	if summary.Alert.Name != "go2/motor/fr-thigh" || summary.AlertThreshold != 70 {
		t.Fatalf("alert = %+v @ %v, want Go2 motor @ 70C", summary.Alert, summary.AlertThreshold)
	}
}

func TestSummarizeTemperatureThresholdBoundaries(t *testing.T) {
	tests := []struct {
		name string
		zone *agentpb.ThermalZone
		want thermalRisk
	}{
		{name: "motor below near band", zone: &agentpb.ThermalZone{Name: "go2/motor/fl-calf", TempC: 64.9}, want: thermalNormal},
		{name: "motor at near edge", zone: &agentpb.ThermalZone{Name: "go2/motor/fl-calf", TempC: 65}, want: thermalNear},
		{name: "motor at warning", zone: &agentpb.ThermalZone{Name: "go2/motor/fl-calf", TempC: 70}, want: thermalOver},
		{name: "imu at near edge", zone: &agentpb.ThermalZone{Name: "go2/imu", TempC: 80}, want: thermalNear},
		{name: "generic zone at near edge", zone: &agentpb.ThermalZone{Name: "cpu-thermal", TempC: 80}, want: thermalNear},
		{name: "unknown Go2 sensor has no guessed threshold", zone: &agentpb.ThermalZone{Name: "go2/unknown", TempC: 100}, want: thermalNormal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary, ok := summarizeTemperature(&agentpb.HostStats{ThermalZones: []*agentpb.ThermalZone{tt.zone}})
			if !ok || summary.Risk != tt.want {
				t.Fatalf("summarizeTemperature(%+v) = risk %v, ok %v; want %v", tt.zone, summary.Risk, ok, tt.want)
			}
		})
	}
}

func TestSummarizeTemperatureIncludesGPUOnlyReading(t *testing.T) {
	temp := 81.0
	summary, ok := summarizeTemperature(&agentpb.HostStats{Gpus: []*agentpb.GpuStats{{Index: 2, TempC: &temp}}})
	if !ok || summary.Max.Name != "gpu/2" || summary.Max.TempC != 81 || summary.Risk != thermalNear {
		t.Fatalf("GPU-only summary = %+v, ok %v", summary, ok)
	}
}

func TestRenderTemperatureHeaderShowsCircleOnlyForAlert(t *testing.T) {
	normal := renderTemperatureHeader(temperatureSummary{Max: thermalReading{Name: "go2/imu", TempC: 79}})
	if strings.Contains(normal, "●") {
		t.Fatalf("normal header unexpectedly has an alert circle: %q", normal)
	}
	near := renderTemperatureHeader(temperatureSummary{
		Max:            thermalReading{Name: "go2/motor/fr-thigh", TempC: 66},
		Risk:           thermalNear,
		Alert:          thermalReading{Name: "go2/motor/fr-thigh", TempC: 66},
		AlertThreshold: 70,
	})
	for _, want := range []string{"●", "Temp: max", "66°C", "near 70°C warning"} {
		if !strings.Contains(near, want) {
			t.Fatalf("near header missing %q: %q", want, near)
		}
	}
}

func TestHostCPUPercent(t *testing.T) {
	prev := topSample{host: &agentpb.HostStats{CpuTotalJiffies: 1000, CpuIdleJiffies: 800}}
	cur := topSample{host: &agentpb.HostStats{CpuTotalJiffies: 1100, CpuIdleJiffies: 850}}
	// totalΔ=100, idleΔ=50 → busy = 1 - 50/100 = 50%
	if got := hostCPUPercent(prev, cur); !approx(got, 50) {
		t.Errorf("hostCPUPercent = %v, want 50", got)
	}
}

func TestHostCPUPercentNoDelta(t *testing.T) {
	s := topSample{host: &agentpb.HostStats{CpuTotalJiffies: 1000, CpuIdleJiffies: 800}}
	if got := hostCPUPercent(s, s); got != 0 {
		t.Errorf("hostCPUPercent = %v, want 0", got)
	}
}

func TestContainerCPUPercent(t *testing.T) {
	// 1e9 ns of CPU over 1e9 ns of wall time on a 2-core machine = 50% of machine.
	prev := topSample{containers: map[string]uint64{"a": 0}, takenAtNanos: 0}
	cur := topSample{containers: map[string]uint64{"a": 1_000_000_000}, takenAtNanos: 1_000_000_000}
	if got := containerCPUPercent(prev, cur, "a", 2); !approx(got, 50) {
		t.Errorf("containerCPUPercent = %v, want 50", got)
	}
}

func TestBuildTopRowsSortedByMemoryDesc(t *testing.T) {
	containers := []*agentpb.AppContainer{
		{AppName: "low", RunningState: agentpb.AppRunningState_RUNNING},
		{AppName: "high", RunningState: agentpb.AppRunningState_RUNNING},
	}
	mem := map[string]int64{"low": 100, "high": 900}
	cpu := map[string]float64{"low": 1, "high": 2}
	rows := buildTopRows(containers, cpu, mem, false /*sortByCPU*/)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].name != "high" {
		t.Errorf("first row = %q, want high (mem desc)", rows[0].name)
	}
}

func TestBuildTopRowsMultiServiceGrouping(t *testing.T) {
	containers := []*agentpb.AppContainer{
		{
			AppName:      "web",
			RunningState: agentpb.AppRunningState_RUNNING,
			Services: []*agentpb.ServiceEntry{
				{Name: "api", RunningState: agentpb.AppRunningState_RUNNING},
				{Name: "worker", RunningState: agentpb.AppRunningState_RUNNING},
			},
		},
	}
	// Per-service stats are keyed appID_serviceName.
	mem := map[string]int64{"web_api": 100, "web_worker": 200}
	cpu := map[string]float64{"web_api": 5, "web_worker": 7}
	rows := buildTopRows(containers, cpu, mem, false)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 (header + 2 services)", len(rows))
	}
	if !rows[0].isGroupHeader {
		t.Errorf("row 0 should be group header")
	}
	if rows[0].memBytes != 300 {
		t.Errorf("group mem = %d, want 300", rows[0].memBytes)
	}
	if !rows[1].isSubrow || !rows[2].isSubrow {
		t.Errorf("rows 1,2 should be subrows")
	}
	if rows[0].state != agentpb.AppRunningState_RUNNING || rows[1].state != agentpb.AppRunningState_RUNNING {
		t.Errorf("running states not preserved in rows: %+v", rows)
	}
}

func TestTopStateLabelCrashLoopingIsCompact(t *testing.T) {
	if got := topStateLabel(agentpb.AppRunningState_CRASH_LOOPING); got != "crash-loop" {
		t.Fatalf("topStateLabel(CRASH_LOOPING) = %q, want crash-loop", got)
	}
}

func TestBuildTopJSON(t *testing.T) {
	containers := []*agentpb.AppContainer{
		{AppName: "myapp", RunningState: agentpb.AppRunningState_RUNNING},
	}
	prev := topSample{
		host:         &agentpb.HostStats{CpuTotalJiffies: 1000, CpuIdleJiffies: 900, CpuCount: 2, MemTotalBytes: 200, MemAvailableBytes: 150},
		containers:   map[string]uint64{"myapp": 0},
		mem:          map[string]int64{"myapp": 50},
		takenAtNanos: 0,
	}
	cur := topSample{
		host:         &agentpb.HostStats{CpuTotalJiffies: 1100, CpuIdleJiffies: 950, CpuCount: 2, MemTotalBytes: 200, MemAvailableBytes: 140},
		containers:   map[string]uint64{"myapp": 500_000_000},
		mem:          map[string]int64{"myapp": 60},
		takenAtNanos: 1_000_000_000,
	}
	out := buildTopJSON(prev, cur, containers)
	if out.Host.CPUPercent <= 0 {
		t.Errorf("host cpu%% = %v, want > 0", out.Host.CPUPercent)
	}
	if out.Host.MemUsedBytes != 60 { // total - available = 200-140
		t.Errorf("host memUsed = %d, want 60", out.Host.MemUsedBytes)
	}
	if len(out.Containers) != 1 || out.Containers[0].Name != "myapp" {
		t.Fatalf("containers = %+v", out.Containers)
	}
	if out.Containers[0].CPUPercent <= 0 {
		t.Errorf("container cpu%% = %v, want > 0", out.Containers[0].CPUPercent)
	}
	if out.Containers[0].State != "running" {
		t.Errorf("container state = %q, want running", out.Containers[0].State)
	}
}

func TestBuildTopJSONIncludesMaximumTemperatureAdditively(t *testing.T) {
	temp := 84.0
	sample := topSample{host: &agentpb.HostStats{
		ThermalZones: []*agentpb.ThermalZone{{Name: "cpu-thermal", TempC: 72}},
		Gpus:         []*agentpb.GpuStats{{Index: 0, TempC: &temp}},
	}}
	out := buildTopJSON(sample, sample, nil)
	if out.Host.MaximumTemperature == nil {
		t.Fatal("expected maximumTemperature")
	}
	if out.Host.MaximumTemperature.Name != "gpu/0" || out.Host.MaximumTemperature.TempC != 84 {
		t.Fatalf("maximumTemperature = %+v, want gpu/0 at 84C", out.Host.MaximumTemperature)
	}
	if len(out.Host.ThermalZones) != 1 || out.Host.ThermalZones[0].Name != "cpu-thermal" {
		t.Fatalf("existing thermalZones changed: %+v", out.Host.ThermalZones)
	}

	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"maximumTemperature":{"name":"gpu/0","tempC":84}`, `"thermalZones":[{"name":"cpu-thermal","tempC":72}]`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("JSON missing additive field %s: %s", want, data)
		}
	}
}

func TestBuildTopJSONOmitsMaximumTemperatureWithoutReading(t *testing.T) {
	sample := topSample{host: &agentpb.HostStats{}}
	data, err := json.Marshal(buildTopJSON(sample, sample, nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "maximumTemperature") {
		t.Fatalf("empty host must omit maximumTemperature: %s", data)
	}
}

func TestWriteTopPlainSnapshotIncludesStateAndInactiveMetrics(t *testing.T) {
	containers := []*agentpb.AppContainer{
		{AppName: "active", RunningState: agentpb.AppRunningState_RUNNING},
		{AppName: "idle", RunningState: agentpb.AppRunningState_STOPPED},
	}
	prev := topSample{
		host:         &agentpb.HostStats{CpuCount: 2},
		containers:   map[string]uint64{"active": 0},
		mem:          map[string]int64{"active": 25},
		takenAtNanos: 0,
	}
	cur := topSample{
		host:         &agentpb.HostStats{CpuCount: 2},
		containers:   map[string]uint64{"active": 500_000_000},
		mem:          map[string]int64{"active": 25},
		takenAtNanos: 1_000_000_000,
	}

	var out bytes.Buffer
	if err := writeTopPlainSnapshot(&out, prev, cur, containers); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "STATE") || !strings.Contains(text, "active") || !strings.Contains(text, "running") {
		t.Fatalf("plain snapshot does not show running state:\n%s", text)
	}
	if !strings.Contains(text, "idle") || !strings.Contains(text, "stopped") {
		t.Fatalf("plain snapshot does not show stopped state:\n%s", text)
	}
	idleLine := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "idle") {
			idleLine = line
			break
		}
	}
	if strings.Contains(idleLine, "0.0") || strings.Contains(idleLine, "0 B") {
		t.Fatalf("stopped app should show unavailable resource values, got %q", idleLine)
	}
}

func TestWriteTopPlainSnapshotIncludesMaximumTemperature(t *testing.T) {
	sample := topSample{host: &agentpb.HostStats{
		ThermalZones: []*agentpb.ThermalZone{{Name: "go2/motor/rr-thigh", TempC: 67}},
	}}
	var out bytes.Buffer
	if err := writeTopPlainSnapshot(&out, sample, sample, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"TEMP MAX: 67°C (Go2 rr thigh)", "near 70°C warning"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("plain snapshot missing %q:\n%s", want, out.String())
		}
	}
}

func TestTopViewMakesAllAppStatesExplicit(t *testing.T) {
	m := topModel{
		width:  100,
		height: 24,
		cur: topSample{host: &agentpb.HostStats{MemTotalBytes: 100}, mem: map[string]int64{
			"active": 25,
		}},
		havePrev: true,
		cachedContainers: []*agentpb.AppContainer{
			{AppName: "active", RunningState: agentpb.AppRunningState_RUNNING},
			{AppName: "idle", RunningState: agentpb.AppRunningState_STOPPED},
			{AppName: "broken", RunningState: agentpb.AppRunningState_CRASH_LOOPING},
		},
	}
	m.rebuildRows()
	view := m.View()
	for _, want := range []string{"● active", "running", "○ idle", "stopped", "↻ broken", "crash-loop"} {
		if !strings.Contains(view, want) {
			t.Fatalf("top view missing %q:\n%s", want, view)
		}
	}
	for _, want := range []string{"1 running", "1 stopped", "1 crash-looping"} {
		if !strings.Contains(view, want) {
			t.Fatalf("top summary missing %q:\n%s", want, view)
		}
	}
}

func TestTopViewPutsThermalWarningInHeader(t *testing.T) {
	m := topModel{
		width:    100,
		height:   24,
		havePrev: true,
		storage:  &agentpb.DiskPartition{Mountpoint: "/data", TotalBytes: 100, UsedBytes: 20},
		cur: topSample{host: &agentpb.HostStats{
			MemTotalBytes: 100,
			ThermalZones: []*agentpb.ThermalZone{
				{Name: "go2/imu", TempC: 79},
				{Name: "go2/motor/fr-thigh", TempC: 66},
			},
		}},
	}
	view := m.View()
	var temperatureLine string
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "Temp:") {
			temperatureLine = line
		}
	}
	for _, want := range []string{"●", "Temp: max", "79°C", "Go2 fr thigh 66°C", "near 70°C warning"} {
		if !strings.Contains(temperatureLine, want) {
			t.Fatalf("thermal header missing %q:\n%s", want, view)
		}
	}
	if strings.Count(view, "Temp:") != 1 || strings.Index(view, "Disk /data") > strings.Index(view, "Temp:") {
		t.Fatalf("expected one temperature line below disk usage:\n%s", view)
	}
}

func TestTopViewKeepsStateVisibleAtSeventyColumns(t *testing.T) {
	m := topModel{
		width:            70,
		height:           18,
		havePrev:         true,
		cur:              topSample{host: &agentpb.HostStats{MemTotalBytes: 100}},
		cachedContainers: []*agentpb.AppContainer{{AppName: "idle", RunningState: agentpb.AppRunningState_STOPPED}},
	}
	m.rebuildRows()
	view := m.View()
	if !strings.Contains(view, "○ idle") || !strings.Contains(view, "stopped") {
		t.Fatalf("state was cropped at 70 columns:\n%s", view)
	}
	if strings.Contains(view, "OPEN PORTS") {
		t.Fatalf("ports panel should yield to the state table at 70 columns:\n%s", view)
	}
}

func TestTopStopKeyStopsParentAppFromServiceRow(t *testing.T) {
	fake := &topFakeContainerClient{}
	conn := &grpcclient.AgentConnection{ContainerService: fake}
	m := newTopModel(context.Background(), conn, 2*time.Second)
	m.rows = []topRow{
		{name: "group-app", displayName: "group-app [group]", state: agentpb.AppRunningState_RUNNING},
		{displayName: "  ↳ worker", state: agentpb.AppRunningState_RUNNING, isSubrow: true},
	}
	m.cursor = 1

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = updated.(topModel)
	if cmd == nil || m.stoppingApp != "group-app" || !strings.Contains(m.actionStatus, "Stopping group-app") {
		t.Fatalf("stop did not enter pending state: stopping=%q status=%q cmd=%v", m.stoppingApp, m.actionStatus, cmd != nil)
	}

	msg := cmd()
	updated, _ = m.Update(msg)
	m = updated.(topModel)
	if len(fake.stopCalls) != 1 || fake.stopCalls[0] != "group-app" {
		t.Fatalf("StopContainer calls = %v, want [group-app]", fake.stopCalls)
	}
	if m.stoppingApp != "" || m.actionStatus != "Stopped group-app" {
		t.Fatalf("stop result state: stopping=%q status=%q", m.stoppingApp, m.actionStatus)
	}
}

func TestTopStopKeyDoesNotCallAlreadyStoppedApp(t *testing.T) {
	fake := &topFakeContainerClient{}
	m := newTopModel(context.Background(), &grpcclient.AgentConnection{ContainerService: fake}, 2*time.Second)
	m.rows = []topRow{{name: "idle", displayName: "idle", state: agentpb.AppRunningState_STOPPED}}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = updated.(topModel)
	if cmd == nil {
		t.Fatal("already-stopped notice should schedule its own cleanup")
	}
	if len(fake.stopCalls) != 0 || m.actionStatus != "idle is already stopped" {
		t.Fatalf("unexpected stopped-app action: calls=%v status=%q", fake.stopCalls, m.actionStatus)
	}
}

func TestTopSuccessfulPollDoesNotClearStopStatus(t *testing.T) {
	m := topModel{
		actionStatus: "Stopping app…",
		cur:          topSample{host: &agentpb.HostStats{}},
		statsCh:      make(chan topStatsMsg),
	}
	updated, _ := m.Update(topStatsMsg{resp: &agentpb.GetResourceStatsResponse{}})
	m = updated.(topModel)
	if m.actionStatus != "Stopping app…" {
		t.Fatalf("successful poll cleared lifecycle status: %q", m.actionStatus)
	}
}

// Jetson unified memory: the agent leaves GPU mem fields zero because
// nvidia-smi answers "[N/A]". The JSON must omit them and the text renderers
// must say "shared" instead of "0 B / 0 B" (WDY-1808).
func TestBuildTopJSON_GPUMemUnsetOmitted(t *testing.T) {
	mkSample := func() topSample {
		return topSample{
			host: &agentpb.HostStats{
				CpuCount: 2, MemTotalBytes: 200, MemAvailableBytes: 140,
				Gpus: []*agentpb.GpuStats{{Name: "NVIDIA Thor", UtilPercent: 85}},
			},
		}
	}
	out := buildTopJSON(mkSample(), mkSample(), nil)
	if len(out.Host.GPUs) != 1 {
		t.Fatalf("gpus = %d, want 1", len(out.Host.GPUs))
	}
	g := out.Host.GPUs[0]
	if g.MemUsedBytes != 0 || g.MemTotalBytes != 0 {
		t.Errorf("gpu mem = %d/%d, want 0/0", g.MemUsedBytes, g.MemTotalBytes)
	}

	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "memTotalBytes\":0") || strings.Contains(string(data), "memUsedBytes\":0") {
		t.Errorf("JSON renders unset GPU memory as 0: %s", data)
	}
	// The host memory keys must be unaffected by the GPU omission.
	if !strings.Contains(string(data), `"memTotalBytes":200`) {
		t.Errorf("host memTotalBytes missing from JSON: %s", data)
	}
}

func TestFormatGPUMem(t *testing.T) {
	got := formatGPUMem(&agentpb.GpuStats{MemUsedBytes: 1 << 30, MemTotalBytes: 6 << 30})
	if !strings.Contains(got, "/") || strings.Contains(got, "shared") {
		t.Errorf("formatGPUMem(set) = %q, want used / total", got)
	}
	if got := formatGPUMem(&agentpb.GpuStats{}); got != "shared" {
		t.Errorf("formatGPUMem(unset) = %q, want %q", got, "shared")
	}
}

// --- battery ---

// batterySample builds a top sample whose host carries the given battery (nil
// for a mains-powered device).
func batterySample(b *agentpb.BatteryStats) topSample {
	return topSample{
		host: &agentpb.HostStats{CpuCount: 2, MemTotalBytes: 200, MemAvailableBytes: 140, Battery: b},
	}
}

func dischargingBattery() *agentpb.BatteryStats {
	remaining := int64(8040)
	return &agentpb.BatteryStats{
		Percent:          78,
		State:            agentpb.BatteryState_BATTERY_STATE_DISCHARGING,
		SecondsRemaining: &remaining,
	}
}

func TestBuildTopJSON_BatteryOmittedWithoutOne(t *testing.T) {
	out := buildTopJSON(batterySample(nil), batterySample(nil), nil)
	if out.Host.Battery != nil {
		t.Fatalf("mains-powered device must report no battery, got %+v", out.Host.Battery)
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "battery") {
		t.Errorf("JSON must omit the battery key entirely: %s", data)
	}
}

func TestBuildTopJSON_Battery(t *testing.T) {
	out := buildTopJSON(batterySample(dischargingBattery()), batterySample(dischargingBattery()), nil)
	if out.Host.Battery == nil {
		t.Fatal("expected battery in JSON output")
	}
	if out.Host.Battery.Percent != 78 {
		t.Errorf("percent = %v, want 78", out.Host.Battery.Percent)
	}
	if out.Host.Battery.State != "discharging" {
		t.Errorf("state = %q, want discharging", out.Host.Battery.State)
	}
	if out.Host.Battery.SecondsRemaining == nil || *out.Host.Battery.SecondsRemaining != 8040 {
		t.Errorf("secondsRemaining = %v, want 8040", out.Host.Battery.SecondsRemaining)
	}
}

func TestBuildTopJSON_BatteryEstimateOmittedWhenUnknown(t *testing.T) {
	b := &agentpb.BatteryStats{Percent: 64, State: agentpb.BatteryState_BATTERY_STATE_DISCHARGING}
	out := buildTopJSON(batterySample(b), batterySample(b), nil)
	if out.Host.Battery == nil {
		t.Fatal("expected battery in JSON output")
	}
	if out.Host.Battery.SecondsRemaining != nil {
		t.Errorf("secondsRemaining = %v, want absent", *out.Host.Battery.SecondsRemaining)
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secondsRemaining") {
		t.Errorf("JSON must omit an unknown estimate rather than send 0: %s", data)
	}
}

func TestWriteTopPlainSnapshot_BatteryLine(t *testing.T) {
	var out bytes.Buffer
	s := batterySample(dischargingBattery())
	if err := writeTopPlainSnapshot(&out, s, s, nil); err != nil {
		t.Fatal(err)
	}
	if want := "BAT: 78% (discharging, 2h14m left)"; !strings.Contains(out.String(), want) {
		t.Errorf("plain snapshot missing %q:\n%s", want, out.String())
	}
}

func TestWriteTopPlainSnapshot_NoBatteryNoLine(t *testing.T) {
	var out bytes.Buffer
	s := batterySample(nil)
	if err := writeTopPlainSnapshot(&out, s, s, nil); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(strings.ToUpper(text), "BAT") {
		t.Errorf("mains-powered device must add nothing to the snapshot:\n%s", text)
	}
	// The rest of the snapshot is untouched.
	if !strings.Contains(text, "CPU:") || !strings.Contains(text, "MEM:") {
		t.Errorf("snapshot lost its existing host lines:\n%s", text)
	}
}

func TestTopView_ShowsBatteryMeter(t *testing.T) {
	m := topModel{width: 100, height: 24, havePrev: true, cur: batterySample(dischargingBattery())}
	m.rebuildRows()
	view := m.View()
	for _, want := range []string{"Bat", "78%", "discharging", "2h14m"} {
		if !strings.Contains(view, want) {
			t.Fatalf("top view missing %q:\n%s", want, view)
		}
	}
}

func TestTopView_NoBatteryMeterWithoutOne(t *testing.T) {
	m := topModel{width: 100, height: 24, havePrev: true, cur: batterySample(nil)}
	m.rebuildRows()
	view := m.View()
	if strings.Contains(view, "Bat[") {
		t.Fatalf("mains-powered device must show no battery meter:\n%s", view)
	}
	// The CPU and Mem meters still render.
	if !strings.Contains(view, "CPU") || !strings.Contains(view, "Mem[") {
		t.Fatalf("top view lost its existing meters:\n%s", view)
	}
}

func TestTopKeyBarFitsNarrowTerminals(t *testing.T) {
	for _, width := range []int{1, 20, 40, 57, 80} {
		bar := (topModel{}).topKeyBar(width)
		if got := visibleWidth(bar); got != width {
			t.Fatalf("width %d rendered %d cells", width, got)
		}
	}
}

func TestTopDiskMountpointSanitizedForTerminal(t *testing.T) {
	storage := &agentpb.DiskPartition{Mountpoint: "/data\x1b[2J\r\u202e", TotalBytes: 100, UsedBytes: 20}
	m := topModel{width: 100, height: 24, storage: storage, actionStatus: "Started app\x1b[2J\r\u202e"}
	var plain bytes.Buffer
	if err := writeTopPlainSnapshot(&plain, topSample{}, topSample{storage: storage}, nil); err != nil {
		t.Fatal(err)
	}
	for _, view := range []string{m.View(), plain.String()} {
		for _, control := range []string{"\x1b[2J", "\r", "\u202e"} {
			if strings.Contains(view, control) {
				t.Fatalf("mountpoint control survived: %q", view)
			}
		}
		if !strings.Contains(view, "/data") {
			t.Fatal("mountpoint text missing")
		}
	}
}
