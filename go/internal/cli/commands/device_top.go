package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// topSample is a normalized snapshot used to compute CPU% from deltas.
type topSample struct {
	storage      *agentpb.DiskPartition
	host         *agentpb.HostStats
	containers   map[string]uint64 // container ID -> cumulative cpu nanos
	mem          map[string]int64  // container ID -> memory bytes
	takenAtNanos int64
}

func newTopSample(resp *agentpb.GetResourceStatsResponse, atNanos int64) topSample {
	s := topSample{
		host:         resp.GetHost(),
		containers:   make(map[string]uint64),
		mem:          make(map[string]int64),
		takenAtNanos: atNanos,
	}
	for _, c := range resp.GetContainers() {
		s.containers[c.GetAppName()] = c.GetCpuUsageNanos()
		s.mem[c.GetAppName()] = c.GetMemoryBytes()
	}
	return s
}

// hostCPUPercent returns busy CPU percentage (0-100) across the whole machine,
// computed from the idle/total jiffy deltas between two samples.
func hostCPUPercent(prev, cur topSample) float64 {
	if prev.host == nil || cur.host == nil {
		return 0
	}
	totalΔ := int64(cur.host.GetCpuTotalJiffies()) - int64(prev.host.GetCpuTotalJiffies())
	idleΔ := int64(cur.host.GetCpuIdleJiffies()) - int64(prev.host.GetCpuIdleJiffies())
	if totalΔ <= 0 {
		return 0
	}
	busy := (1 - float64(idleΔ)/float64(totalΔ)) * 100
	if busy < 0 {
		return 0
	}
	return busy
}

// containerCPUPercent returns a container's CPU usage as a percentage of the
// whole machine (0-100 across all cores), from the CPU-nanos delta over elapsed
// wall time. cpuCount normalizes to "share of total machine".
func containerCPUPercent(prev, cur topSample, id string, cpuCount uint32) float64 {
	wallΔ := cur.takenAtNanos - prev.takenAtNanos
	if wallΔ <= 0 || cpuCount == 0 {
		return 0
	}
	prevNanos, ok := prev.containers[id]
	if !ok {
		return 0
	}
	curNanos := cur.containers[id]
	if curNanos < prevNanos {
		return 0 // counter reset / container restarted
	}
	pct := float64(curNanos-prevNanos) / (float64(wallΔ) * float64(cpuCount)) * 100
	if pct < 0 {
		return 0
	}
	return pct
}

// topRow is one display row (app, group header, or service subrow).
type topRow struct {
	name          string // app ID; "" for subrows
	displayName   string
	cpuPercent    float64
	memBytes      int64
	state         agentpb.AppRunningState
	hasCPU        bool
	isGroupHeader bool
	isSubrow      bool
}

// topStateLabel renders an app/service running state as a short lowercase label.
func topStateLabel(s agentpb.AppRunningState) string {
	switch s {
	case agentpb.AppRunningState_RUNNING:
		return "running"
	case agentpb.AppRunningState_STOPPED:
		return "stopped"
	case agentpb.AppRunningState_CRASH_LOOPING:
		return "crash-loop"
	default:
		return strings.ToLower(strings.TrimPrefix(s.String(), "APP_RUNNING_STATE_"))
	}
}

func topStateIcon(s agentpb.AppRunningState) string {
	switch s {
	case agentpb.AppRunningState_RUNNING:
		return "●"
	case agentpb.AppRunningState_CRASH_LOOPING:
		return "↻"
	default:
		return "○"
	}
}

func topDisplayName(r topRow) string {
	return topStateIcon(r.state) + " " + r.displayName
}

// buildTopRows groups containers by app (mirroring buildDashboardRows) with CPU%
// and memory columns. Top-level apps are sorted by the active key descending;
// service subrows stay under their group header.
func buildTopRows(containers []*agentpb.AppContainer, cpuByID map[string]float64, memByID map[string]int64, sortByCPU bool) []topRow {
	type appAgg struct {
		container *agentpb.AppContainer
		cpu       float64
		mem       int64
	}
	aggs := make([]appAgg, 0, len(containers))
	for _, c := range containers {
		appName := c.GetAppName()
		var cpu float64
		var mem int64
		if len(c.GetServices()) > 1 {
			for _, svc := range c.GetServices() {
				key := appName + "_" + svc.GetName()
				cpu += cpuByID[key]
				mem += memByID[key]
			}
		} else {
			cpu = cpuByID[appName]
			mem = memByID[appName]
		}
		aggs = append(aggs, appAgg{container: c, cpu: cpu, mem: mem})
	}

	sort.SliceStable(aggs, func(i, j int) bool {
		if sortByCPU {
			return aggs[i].cpu > aggs[j].cpu
		}
		return aggs[i].mem > aggs[j].mem
	})

	var rows []topRow
	for _, a := range aggs {
		c := a.container
		appName := c.GetAppName()
		appState := c.GetRunningState()
		if len(c.GetServices()) > 1 {
			rows = append(rows, topRow{
				name:          appName,
				displayName:   appName + " [group]",
				cpuPercent:    a.cpu,
				memBytes:      a.mem,
				state:         appState,
				hasCPU:        appState == agentpb.AppRunningState_RUNNING,
				isGroupHeader: true,
			})
			for _, svc := range c.GetServices() {
				key := appName + "_" + svc.GetName()
				svcState := svc.GetRunningState()
				rows = append(rows, topRow{
					displayName: "  ↳ " + svc.GetName(),
					cpuPercent:  cpuByID[key],
					memBytes:    memByID[key],
					state:       svcState,
					hasCPU:      svcState == agentpb.AppRunningState_RUNNING,
					isSubrow:    true,
				})
			}
		} else {
			rows = append(rows, topRow{
				name:        appName,
				displayName: appName,
				cpuPercent:  a.cpu,
				memBytes:    a.mem,
				state:       appState,
				hasCPU:      appState == agentpb.AppRunningState_RUNNING,
			})
		}
	}
	return rows
}

// errResourceStatsUnimplemented marks an agent too old to support device top.
var errResourceStatsUnimplemented = fmt.Errorf("the device's agent does not support resource stats; update it with 'wendy device update'")

func sampleResourceStats(ctx context.Context, conn *grpcclient.AgentConnection) (*agentpb.GetResourceStatsResponse, error) {
	resp, err := conn.ContainerService.GetResourceStats(ctx, &agentpb.GetResourceStatsRequest{})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return nil, errResourceStatsUnimplemented
		}
		return nil, err
	}
	return resp, nil
}

func listAppContainers(ctx context.Context, conn *grpcclient.AgentConnection) ([]*agentpb.AppContainer, error) {
	stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		return nil, err
	}
	var out []*agentpb.AppContainer
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if c := resp.GetContainer(); c != nil {
			out = append(out, c)
		}
	}
	return out, nil
}

type topJSONStorage struct {
	Mountpoint string `json:"mountpoint"`
	UsedBytes  int64  `json:"usedBytes"`
	TotalBytes int64  `json:"totalBytes"`
}

type topJSONHost struct {
	ContainerStorage   *topJSONStorage  `json:"containerStorage,omitempty"`
	CPUPercent         float64          `json:"cpuPercent"`
	CPUCount           uint32           `json:"cpuCount"`
	MemUsedBytes       int64            `json:"memUsedBytes"`
	MemTotalBytes      int64            `json:"memTotalBytes"`
	GPUs               []topJSONGPU     `json:"gpus,omitempty"`
	ThermalZones       []topJSONThermal `json:"thermalZones,omitempty"`
	MaximumTemperature *topJSONThermal  `json:"maximumTemperature,omitempty"`
	// Absent on mains-powered devices. Consumers must read "no battery key" as
	// "no battery", never as a flat one.
	Battery *topJSONBattery `json:"battery,omitempty"`
}

type topJSONThermal struct {
	Name  string  `json:"name"`
	TempC float64 `json:"tempC"`
}

type topJSONBattery struct {
	Percent float64 `json:"percent"`
	State   string  `json:"state"`
	// Absent when the device reports no usable charge/discharge rate.
	SecondsRemaining *int64 `json:"secondsRemaining,omitempty"`
}

type topJSONGPU struct {
	Index       uint32  `json:"index"`
	Name        string  `json:"name"`
	UtilPercent float64 `json:"utilPercent"`
	// Omitted (zero) when the device cannot report per-GPU memory (e.g.
	// Jetson unified memory, where the GPU shares host RAM) — absent means
	// "not applicable", never a real size.
	MemUsedBytes  int64    `json:"memUsedBytes,omitempty"`
	MemTotalBytes int64    `json:"memTotalBytes,omitempty"`
	TempC         *float64 `json:"tempC,omitempty"`
	PowerW        *float64 `json:"powerW,omitempty"`
}

type topJSONContainer struct {
	Name       string  `json:"name"`
	State      string  `json:"state"`
	CPUPercent float64 `json:"cpuPercent"`
	MemBytes   int64   `json:"memBytes"`
}

type topJSONOutput struct {
	Host       topJSONHost        `json:"host"`
	Containers []topJSONContainer `json:"containers"`
}

func buildTopJSON(prev, cur topSample, containers []*agentpb.AppContainer) topJSONOutput {
	out := topJSONOutput{}
	if cur.storage != nil {
		out.Host.ContainerStorage = &topJSONStorage{Mountpoint: cur.storage.GetMountpoint(), UsedBytes: cur.storage.GetUsedBytes(), TotalBytes: cur.storage.GetTotalBytes()}
	}
	if cur.host != nil {
		out.Host.CPUPercent = hostCPUPercent(prev, cur)
		out.Host.CPUCount = cur.host.GetCpuCount()
		out.Host.MemTotalBytes = cur.host.GetMemTotalBytes()
		out.Host.MemUsedBytes = cur.host.GetMemTotalBytes() - cur.host.GetMemAvailableBytes()
		for _, g := range cur.host.GetGpus() {
			out.Host.GPUs = append(out.Host.GPUs, topJSONGPU{
				Index:         g.GetIndex(),
				Name:          g.GetName(),
				UtilPercent:   g.GetUtilPercent(),
				MemUsedBytes:  g.GetMemUsedBytes(),
				MemTotalBytes: g.GetMemTotalBytes(),
				TempC:         g.TempC,
				PowerW:        g.PowerW,
			})
		}
		for _, z := range cur.host.GetThermalZones() {
			out.Host.ThermalZones = append(out.Host.ThermalZones, topJSONThermal{
				Name:  z.GetName(),
				TempC: z.GetTempC(),
			})
		}
		if summary, ok := summarizeTemperature(cur.host); ok {
			out.Host.MaximumTemperature = &topJSONThermal{Name: summary.Max.Name, TempC: summary.Max.TempC}
		}
		if b := cur.host.GetBattery(); b != nil {
			out.Host.Battery = &topJSONBattery{
				Percent:          b.GetPercent(),
				State:            batteryStateLabel(b.GetState()),
				SecondsRemaining: b.SecondsRemaining,
			}
		}
	}
	cpuCount := uint32(1)
	if cur.host != nil && cur.host.GetCpuCount() > 0 {
		cpuCount = cur.host.GetCpuCount()
	}
	cpuByID := map[string]float64{}
	for id := range cur.containers {
		cpuByID[id] = containerCPUPercent(prev, cur, id, cpuCount)
	}
	rows := buildTopRows(containers, cpuByID, cur.mem, false)
	for _, r := range rows {
		if r.isSubrow {
			continue
		}
		out.Containers = append(out.Containers, topJSONContainer{
			Name:       r.displayName,
			State:      topStateLabel(r.state),
			CPUPercent: r.cpuPercent,
			MemBytes:   r.memBytes,
		})
	}
	return out
}

// formatGPUMem renders a GPU's memory as "used / total", or "shared" when the
// device reports no per-GPU figure (Jetson unified memory: the GPU shares host
// RAM, so nvidia-smi answers "[N/A]" — the host Mem line is the real number).
func formatGPUMem(g *agentpb.GpuStats) string {
	if g.GetMemTotalBytes() == 0 {
		return "shared"
	}
	return fmt.Sprintf("%s / %s", formatBytes(g.GetMemUsedBytes()), formatBytes(g.GetMemTotalBytes()))
}

func runTopSnapshot(ctx context.Context, conn *grpcclient.AgentConnection, asJSON bool) error {
	containers, err := listAppContainers(ctx, conn)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	first, err := sampleResourceStats(ctx, conn)
	if err != nil {
		return err
	}
	prev := newTopSample(first, time.Now().UnixNano())
	time.Sleep(250 * time.Millisecond)
	second, err := sampleResourceStats(ctx, conn)
	if err != nil {
		return err
	}
	cur := newTopSample(second, time.Now().UnixNano())
	cur.storage, _ = fetchTopStorage(ctx, conn)

	if asJSON {
		data, err := json.MarshalIndent(buildTopJSON(prev, cur, containers), "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	return writeTopPlainSnapshot(os.Stdout, prev, cur, containers)
}

func writeTopPlainSnapshot(w io.Writer, prev, cur topSample, containers []*agentpb.AppContainer) error {
	if cur.storage != nil {
		fmt.Fprintf(w, "DISK %s: %s / %s\n", tui.StripControl(cur.storage.GetMountpoint()), formatBytes(cur.storage.GetUsedBytes()), formatBytes(cur.storage.GetTotalBytes()))
	}
	cpuCount := uint32(1)
	if cur.host != nil && cur.host.GetCpuCount() > 0 {
		cpuCount = cur.host.GetCpuCount()
	}
	if cur.host != nil {
		fmt.Fprintf(w, "CPU: %.1f%%  MEM: %s / %s\n",
			hostCPUPercent(prev, cur),
			formatBytes(cur.host.GetMemTotalBytes()-cur.host.GetMemAvailableBytes()),
			formatBytes(cur.host.GetMemTotalBytes()))
		for _, g := range cur.host.GetGpus() {
			fmt.Fprintf(w, "GPU%d %s: %.0f%%  %s\n", g.GetIndex(), g.GetName(),
				g.GetUtilPercent(), formatGPUMem(g))
		}
		if summary, ok := summarizeTemperature(cur.host); ok {
			fmt.Fprintf(w, "TEMP MAX: %s\n", formatTemperatureSummary(summary))
		}
		if zones := cur.host.GetThermalZones(); len(zones) > 0 {
			fmt.Fprintf(w, "TEMP: %s\n", formatThermalZones(zones))
		}
		if b := cur.host.GetBattery(); b != nil {
			fmt.Fprintf(w, "BAT: %s\n", formatBatterySummary(b))
		}
	}
	cpuByID := map[string]float64{}
	for id := range cur.containers {
		cpuByID[id] = containerCPUPercent(prev, cur, id, cpuCount)
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "APP\tSTATE\tCPU%\tMEM")
	for _, r := range buildTopRows(containers, cpuByID, cur.mem, false) {
		cpu, mem := "—", "—"
		if r.state == agentpb.AppRunningState_RUNNING {
			cpu = fmt.Sprintf("%.1f", r.cpuPercent)
			mem = formatBytes(r.memBytes)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.displayName, topStateLabel(r.state), cpu, mem)
	}
	return tw.Flush()
}

func newTopCmd() *cobra.Command {
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "top",
		Short: "Live CPU, memory, disk, GPU, and temperature for the device and its containers",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			if jsonOutput || !isInteractiveTerminal() {
				return runTopSnapshot(ctx, conn, jsonOutput)
			}
			return runTopDashboard(ctx, conn, interval)
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "Refresh interval for the live view")
	return cmd
}

type topStatsMsg struct {
	resp       *agentpb.GetResourceStatsResponse
	err        error
	startedAt  time.Time
	finishedAt time.Time
}

type topContainersMsg struct {
	containers []*agentpb.AppContainer
	err        error
	startedAt  time.Time
	finishedAt time.Time
}

type topModel struct {
	conn     *grpcclient.AgentConnection
	ctx      context.Context
	interval time.Duration

	statsCh      chan topStatsMsg
	containersCh chan topContainersMsg

	prev, cur        topSample
	havePrev         bool
	cachedContainers []*agentpb.AppContainer
	// Keep near-equal temperatures from trading places on every sample. The
	// dashboard preserves this order across refreshes; snapshots remain a
	// stateless representation of the agent response.
	displayThermalZones []*agentpb.ThermalZone

	rows      []topRow
	cursor    int
	sortByCPU bool
	width     int
	height    int
	flash     string

	// Liveness. The stats and container polls run independently, so their
	// results can reach Update out of order. Keep the latest proof on each side
	// so a slow failure cannot overwrite a newer successful reply.
	lastReplyAt       time.Time
	lastUnreachableAt time.Time
	offlineSince      time.Time

	// Lifecycle action state is separate from poll errors so a successful poll
	// cannot erase stop progress or its result.
	actionSeq    uint64
	stoppingApp  string
	startingApp  string
	pruningCache bool
	storage      *agentpb.DiskPartition
	storageErr   error

	logsApp      string
	logsSeq      uint64
	logsCancel   context.CancelFunc
	logsViewport viewport.Model
	logsLines    []string
	logsStatus   string
	actionStatus string

	// Ports for the currently selected app (always-on side panel).
	portsApp string
	ports    []*agentpb.PortEntry
	portsErr error
}

type topPortsMsg struct {
	app   string
	ports []*agentpb.PortEntry
	err   error
}

type topStopResultMsg struct {
	seq uint64
	app string
	err error
}

type topClearActionMsg struct{ seq uint64 }

func newTopModel(ctx context.Context, conn *grpcclient.AgentConnection, interval time.Duration) topModel {
	return topModel{
		conn:         conn,
		ctx:          ctx,
		interval:     interval,
		statsCh:      make(chan topStatsMsg, 2),
		containersCh: make(chan topContainersMsg, 2),
	}
}

func (m topModel) Init() tea.Cmd {
	go m.runStatsPoll()
	go m.runContainersPoll()
	return tea.Batch(waitForTopStats(m.statsCh), waitForTopContainers(m.containersCh), m.fetchStorageCmd())
}

func waitForTopStats(ch chan topStatsMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}
func waitForTopContainers(ch chan topContainersMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

// selectedAppName returns the app the cursor is on, walking up from a service
// subrow to its group header.
func (m topModel) selectedAppName() string {
	for i := m.cursor; i >= 0 && i < len(m.rows); i-- {
		if m.rows[i].name != "" {
			return m.rows[i].name
		}
	}
	return ""
}

func (m topModel) selectedAppState() (agentpb.AppRunningState, bool) {
	for i := m.cursor; i >= 0 && i < len(m.rows); i-- {
		if m.rows[i].name != "" {
			return m.rows[i].state, true
		}
	}
	return agentpb.AppRunningState_STOPPED, false
}

func (m topModel) stopAppCmd(app string, seq uint64) tea.Cmd {
	conn, ctx := m.conn, m.ctx
	return func() tea.Msg {
		if conn == nil || conn.ContainerService == nil {
			return topStopResultMsg{seq: seq, app: app, err: fmt.Errorf("container service unavailable")}
		}
		_, err := conn.ContainerService.StopContainer(ctx, &agentpb.StopContainerRequest{AppName: app})
		return topStopResultMsg{seq: seq, app: app, err: err}
	}
}

func clearTopActionStatus(seq uint64) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(3 * time.Second)
		return topClearActionMsg{seq: seq}
	}
}

// fetchPortsCmd fetches the listening ports for app. The result carries the app
// name so a stale response for a no-longer-selected app can be ignored.
func (m topModel) fetchPortsCmd(app string) tea.Cmd {
	conn, ctx := m.conn, m.ctx
	return func() tea.Msg {
		if app == "" {
			return topPortsMsg{app: app}
		}
		resp, err := conn.ContainerService.GetContainerPorts(ctx, &agentpb.GetContainerPortsRequest{AppName: app})
		if err != nil {
			return topPortsMsg{app: app, err: err}
		}
		return topPortsMsg{app: app, ports: resp.GetPorts()}
	}
}

// maybeFetchPorts issues a ports fetch when the selected app has changed,
// clearing the now-stale panel contents. Returns nil when the selection is
// unchanged. The pointer receiver mutates the addressable model copy held by
// Update.
func (m *topModel) maybeFetchPorts() tea.Cmd {
	sel := m.selectedAppName()
	if sel == m.portsApp {
		return nil
	}
	m.portsApp = sel
	m.ports = nil
	m.portsErr = nil
	return m.fetchPortsCmd(sel)
}

func (m topModel) runStatsPoll() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	// fetch keeps polling on transient errors (a hiccup recovers on the next
	// tick); it only stops when the agent does not implement the RPC at all,
	// since that will never succeed.
	fetch := func() bool {
		// Deadline per poll: a device that lost power leaves the socket
		// black-holed, and an undeadlined call would block here — stalling the
		// ticker too — until gRPC keepalive gives up 15 minutes later.
		startedAt := time.Now()
		ctx, cancel := context.WithTimeout(m.ctx, topPollTimeout(m.interval))
		resp, err := sampleResourceStats(ctx, m.conn)
		cancel()
		finishedAt := time.Now()
		select {
		case m.statsCh <- topStatsMsg{resp: resp, err: err, startedAt: startedAt, finishedAt: finishedAt}:
		case <-m.ctx.Done():
			return false
		}
		return !errors.Is(err, errResourceStatsUnimplemented)
	}
	if !fetch() {
		return
	}
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if !fetch() {
				return
			}
		}
	}
}

func (m topModel) runContainersPoll() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	fetch := func() {
		startedAt := time.Now()
		ctx, cancel := context.WithTimeout(m.ctx, topPollTimeout(m.interval))
		containers, err := listAppContainers(ctx, m.conn)
		cancel()
		finishedAt := time.Now()
		select {
		case m.containersCh <- topContainersMsg{containers: containers, err: err, startedAt: startedAt, finishedAt: finishedAt}:
		case <-m.ctx.Done():
		}
	}
	fetch()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			fetch()
		}
	}
}

// rebuildRows recomputes the displayed rows from the cached samples, keeping
// the cursor within bounds.
func (m *topModel) rebuildRows() {
	selectedApp := m.selectedAppName()
	cpuCount := uint32(1)
	if m.cur.host != nil && m.cur.host.GetCpuCount() > 0 {
		cpuCount = m.cur.host.GetCpuCount()
	}
	cpuByID := map[string]float64{}
	if m.havePrev {
		for id := range m.cur.containers {
			cpuByID[id] = containerCPUPercent(m.prev, m.cur, id, cpuCount)
		}
	}
	m.rows = buildTopRows(m.cachedContainers, cpuByID, m.cur.mem, m.sortByCPU)
	if selectedApp != "" {
		for i, r := range m.rows {
			if r.name == selectedApp {
				m.cursor = i
				break
			}
		}
	}
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case topStorageTickMsg:
		return m, m.fetchStorageCmd()
	case topStorageMsg:
		m.storageErr = msg.err
		if msg.err == nil {
			m.storage = msg.storage
		}
		return m, tea.Tick(10*time.Second, func(time.Time) tea.Msg { return topStorageTickMsg{} })
	case topStartResultMsg:
		if msg.seq != m.actionSeq {
			return m, nil
		}
		m.startingApp = ""
		if msg.err != nil {
			m.actionStatus = fmt.Sprintf("Error starting %s: %s", msg.app, userFacingGRPCError(msg.err))
		} else {
			m.actionStatus = "Started " + msg.app
		}
		return m, clearTopActionStatus(msg.seq)
	case topPruneResultMsg:
		if msg.seq != m.actionSeq {
			return m, nil
		}
		m.pruningCache = false
		if msg.err != nil {
			m.actionStatus = "Error clearing cache: " + userFacingGRPCError(msg.err)
		} else {
			m.actionStatus = msg.text
		}
		return m, clearTopActionStatus(msg.seq)
	case topLogsMsg:
		return m.updateLogs(msg)
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.logsApp != "" {
			m.resizeLogs()
		}
		return m, nil

	case topStatsMsg:
		startedAt, finishedAt := topPollTimes(msg.startedAt, msg.finishedAt)
		if msg.err != nil {
			m.noteOfflineErr(msg.err, startedAt, finishedAt)
			return m, waitForTopStats(m.statsCh)
		}
		m.flash = ""
		m.noteOnline(finishedAt)
		if m.cur.host != nil || len(m.cur.containers) > 0 {
			m.prev = m.cur
			m.havePrev = true
		}
		m.cur = newTopSample(msg.resp, finishedAt.UnixNano())
		if m.cur.host != nil {
			m.displayThermalZones = stabilizeThermalZoneOrder(m.displayThermalZones, m.cur.host.GetThermalZones())
		} else {
			m.displayThermalZones = nil
		}
		m.rebuildRows()
		// Refresh ports for the selected app on every tick so they stay current.
		sel := m.selectedAppName()
		m.portsApp = sel
		return m, tea.Batch(waitForTopStats(m.statsCh), m.fetchPortsCmd(sel))

	case topContainersMsg:
		startedAt, finishedAt := topPollTimes(msg.startedAt, msg.finishedAt)
		if msg.err != nil {
			// Both polls ride the same connection, so this normally fails
			// alongside the stats poll.
			m.noteOfflineErr(msg.err, startedAt, finishedAt)
			return m, waitForTopContainers(m.containersCh)
		}
		// A container list is a reply, so it ends an outage — but it is not a
		// sample, so it does not refresh the age of the meters above it.
		m.noteReachable(finishedAt)
		m.cachedContainers = msg.containers
		m.rebuildRows()
		return m, tea.Batch(waitForTopContainers(m.containersCh), m.maybeFetchPorts())

	case topPortsMsg:
		// Ignore responses for an app that is no longer selected.
		if msg.app == m.selectedAppName() {
			m.ports = msg.ports
			m.portsErr = msg.err
			m.portsApp = msg.app
		}
		return m, nil

	case topStopResultMsg:
		if msg.seq != m.actionSeq {
			return m, nil
		}
		m.stoppingApp = ""
		if msg.err != nil {
			m.actionStatus = fmt.Sprintf("Error stopping %s: %s", msg.app, userFacingGRPCError(msg.err))
		} else {
			m.actionStatus = fmt.Sprintf("Stopped %s", msg.app)
		}
		return m, clearTopActionStatus(msg.seq)

	case topClearActionMsg:
		if msg.seq == m.actionSeq && !m.actionBusy() {
			m.actionStatus = ""
		}
		return m, nil

	case tea.KeyMsg:
		if m.logsApp != "" {
			return m.updateLogsKey(msg)
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "enter":
			return m.openLogs()
		case "s":
			app := m.selectedAppName()
			state, ok := m.selectedAppState()
			if app == "" || !ok || m.actionBusy() {
				return m, nil
			}
			m.actionSeq++
			if state == agentpb.AppRunningState_RUNNING {
				m.actionStatus = app + " is already running"
				return m, clearTopActionStatus(m.actionSeq)
			}
			m.startingApp = app
			m.actionStatus = "Starting " + app + "…"
			return m, m.startAppCmd(app, m.actionSeq)
		case "p":
			if m.actionBusy() {
				return m, nil
			}
			m.actionSeq++
			m.pruningCache = true
			m.actionStatus = "Clearing unused container cache…"
			return m, m.pruneCacheCmd(m.actionSeq)
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, m.maybeFetchPorts()
		case "down", "j":
			if m.cursor < len(m.rows)-1 {
				m.cursor++
			}
			return m, m.maybeFetchPorts()
		case "c":
			m.sortByCPU = true
			m.rebuildRows()
			return m, m.maybeFetchPorts()
		case "m":
			m.sortByCPU = false
			m.rebuildRows()
			return m, m.maybeFetchPorts()
		case "x":
			app := m.selectedAppName()
			state, ok := m.selectedAppState()
			if app == "" || !ok || m.actionBusy() {
				return m, nil
			}
			m.actionSeq++
			if state == agentpb.AppRunningState_STOPPED {
				m.actionStatus = fmt.Sprintf("%s is already stopped", app)
				return m, clearTopActionStatus(m.actionSeq)
			}
			m.stoppingApp = app
			m.actionStatus = fmt.Sprintf("Stopping %s…", app)
			return m, m.stopAppCmd(app, m.actionSeq)
		}
		return m, nil
	}
	return m, nil
}

// --- htop-style rendering ---

var (
	topMeterLabel = lipgloss.NewStyle().Bold(true).Foreground(tui.Emerald400)
	topBracket    = lipgloss.NewStyle().Foreground(tui.ColorDim)
	topValDim     = lipgloss.NewStyle().Foreground(tui.ColorDim)
	topHeaderBar  = lipgloss.NewStyle().Bold(true).Background(tui.Emerald500).Foreground(lipgloss.Color("#02160f"))
	// Bright mint selection bar for strong contrast with the black row text.
	topSelRow      = lipgloss.NewStyle().Background(lipgloss.Color("#9FE2BF")).Foreground(lipgloss.Color("#000000"))
	topRunningRow  = lipgloss.NewStyle().Foreground(tui.Emerald400)
	topCrashRow    = lipgloss.NewStyle().Foreground(tui.Red500)
	topThermalNear = lipgloss.NewStyle().Bold(true).Foreground(tui.Amber500)
	topThermalHot  = lipgloss.NewStyle().Bold(true).Foreground(tui.Red500)
	// Reversed rather than merely red: an offline device is the one condition
	// that must not blend into the meter stack above the app table.
	topOfflineBar = lipgloss.NewStyle().Bold(true).Background(tui.Red500).Foreground(lipgloss.Color("#ffffff"))
	topKeyCap     = lipgloss.NewStyle().Foreground(lipgloss.Color("#02160f")).Background(lipgloss.Color("#d0d0d0"))
	topKeyLabel   = lipgloss.NewStyle().Foreground(lipgloss.Color("#02160f")).Background(tui.Emerald500)
)

// topMeter renders an htop-style bracketed meter: LABEL[|||||      value].
// The fill is colored green/amber/red by load, and value is right-aligned
// inside the bracket.
func topMeter(label string, ratio float64, value string, width int) string {
	return topMeterColored(label, ratio, value, width, loadMeterColor(ratio))
}

// loadMeterColor grades a utilization ratio, where more is worse.
func loadMeterColor(ratio float64) lipgloss.Color {
	switch {
	case ratio < 0.5:
		return tui.Emerald500
	case ratio < 0.85:
		return tui.Amber500
	default:
		return tui.Red500
	}
}

// chargeMeterColor grades a battery charge ratio, where *less* is worse — the
// inverse of every other meter here, so a nearly flat pack reads red rather
// than the reassuring green a load meter would paint it.
func chargeMeterColor(ratio float64) lipgloss.Color {
	switch {
	case ratio < 0.15:
		return tui.Red500
	case ratio < 0.30:
		return tui.Amber500
	default:
		return tui.Emerald500
	}
}

func topMeterColored(label string, ratio float64, value string, width int, c lipgloss.Color) string {
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	valW := lipgloss.Width(value)
	inner := width - lipgloss.Width(label) - 2 // 2 for the brackets
	if inner < valW+1 {
		inner = valW + 1
	}
	barArea := inner - valW
	if barArea < 0 {
		barArea = 0
	}
	filled := int(ratio * float64(barArea))
	if filled > barArea {
		filled = barArea
	}
	bars := lipgloss.NewStyle().Foreground(c).Render(strings.Repeat("|", filled))
	gap := strings.Repeat(" ", barArea-filled)
	return topMeterLabel.Render(label) + topBracket.Render("[") + bars + gap + topValDim.Render(value) + topBracket.Render("]")
}

// topNameWidth returns the flexible name width after reserving the resource and
// state columns. The state column is deliberately wide enough for "crash-loop".
func topNameWidth(width int) int {
	// 1 (lead) + name + 1 + 6 (cpu) + 1 + 10 (mem) +
	// 1 + 10 (state) = name + 30. Keep one spare cell for terminal quirks.
	nameW := width - 31
	if nameW < 10 {
		nameW = 10
	}
	return nameW
}

func topFormatRow(name, cpu, mem, state string, nameW int) string {
	r := []rune(name)
	if len(r) > nameW {
		name = string(r[:nameW])
	}
	return fmt.Sprintf(" %-*s %6s %10s %-10s", nameW, name, cpu, mem, state)
}

func (m topModel) View() string {
	if m.logsApp != "" {
		return m.logsView()
	}
	width := m.width
	if width <= 0 {
		width = 80
	}
	height := m.height
	if height <= 0 {
		height = 24
	}

	// Reserve a right-hand ports panel when the terminal is wide enough.
	panelW := 0
	// The app table needs 47 cells to retain its state column. Only split off
	// the ports panel when both columns fit instead of cropping state at common
	// terminal widths.
	if width >= 84 {
		panelW = 34
	}
	listW := width
	if panelW > 0 {
		listW = width - panelW - 3 // " │ " separator
	}

	var top []string // full-width meters + summary

	// An unreachable device keeps its last sample on screen — there is nothing
	// newer to draw — so the banner is what stops those frozen meters from
	// reading as live telemetry.
	if m.isOffline() {
		var lastBattery *agentpb.BatteryStats
		if m.cur.host != nil {
			lastBattery = m.cur.host.GetBattery()
		}
		headline := topOfflineHeadline(m.silentFor(time.Now()), lastBattery)
		top = append(top, topOfflineBar.Render(padOrCrop(" "+headline, width)))
		if m.cur.host != nil {
			top = append(top, topValDim.Render(" Readings below are the last values received, not live."))
		}
	}

	if m.cur.host != nil {
		h := m.cur.host
		meterW := width - 2
		cpuRatio, cpuVal := 0.0, "—"
		if m.havePrev {
			pct := hostCPUPercent(m.prev, m.cur)
			cpuRatio, cpuVal = pct/100, fmt.Sprintf("%.1f%%", pct)
		}
		cpuLabel := "CPU"
		if h.GetCpuCount() > 0 {
			cpuLabel = fmt.Sprintf("CPU(%d)", h.GetCpuCount())
		}
		top = append(top, topMeter(cpuLabel, cpuRatio, cpuVal, meterW))

		used := h.GetMemTotalBytes() - h.GetMemAvailableBytes()
		memRatio := 0.0
		if h.GetMemTotalBytes() > 0 {
			memRatio = float64(used) / float64(h.GetMemTotalBytes())
		}
		top = append(top, topMeter("Mem", memRatio,
			fmt.Sprintf("%s/%s", formatBytes(used), formatBytes(h.GetMemTotalBytes())), meterW))

		// Only devices that actually have a battery get this row; a
		// mains-powered device shows nothing where it would be.
		if b := h.GetBattery(); b != nil {
			ratio := batteryMeterRatio(b)
			top = append(top, topMeterColored("Bat", ratio, formatBatteryMeterValue(b), meterW, chargeMeterColor(ratio)))
		}

		for _, g := range h.GetGpus() {
			val := fmt.Sprintf("%.0f%%", g.GetUtilPercent())
			if g.GetMemTotalBytes() > 0 {
				val += fmt.Sprintf(" %s/%s", formatBytes(g.GetMemUsedBytes()), formatBytes(g.GetMemTotalBytes()))
			} else {
				// No per-GPU figure exists (Jetson unified memory): the GPU
				// shares host RAM — the Mem meter above is the real number.
				val += " shared"
			}
			if g.TempC != nil {
				val += fmt.Sprintf(" %.0f°C", *g.TempC)
			}
			top = append(top, topMeter("GPU", g.GetUtilPercent()/100, val, meterW))
		}

	} else if !m.isOffline() {
		top = append(top, topValDim.Render(" Connecting…"))
	}

	if m.storage != nil && m.storage.GetTotalBytes() > 0 {
		disk := m.storage
		label := "Disk " + tui.StripControl(disk.GetMountpoint())
		if m.storageErr != nil {
			label += " (stale)"
		}
		top = append(top, topMeter(label, float64(disk.GetUsedBytes())/float64(disk.GetTotalBytes()), fmt.Sprintf("%s/%s", formatBytes(disk.GetUsedBytes()), formatBytes(disk.GetTotalBytes())), width-2))
	} else if m.storageErr != nil {
		top = append(top, topValDim.Render(" Disk: unavailable"))
	}

	if summary, ok := summarizeTemperature(m.cur.host); ok {
		zones := m.displayThermalZones
		// Use the sample's order before Update has established the display order.
		if zones == nil {
			zones = m.cur.host.GetThermalZones()
		}
		top = append(top, renderTemperatureHeader(summary, zones...))
	}

	running, stopped, crashLooping := 0, 0, 0
	for _, c := range m.cachedContainers {
		switch c.GetRunningState() {
		case agentpb.AppRunningState_RUNNING:
			running++
		case agentpb.AppRunningState_CRASH_LOOPING:
			crashLooping++
		default:
			stopped++
		}
	}
	summary := fmt.Sprintf(" Apps: %d  ● %d running  ○ %d stopped", len(m.cachedContainers), running, stopped)
	if crashLooping > 0 {
		summary += fmt.Sprintf("  ↻ %d crash-looping", crashLooping)
	}
	top = append(top, topValDim.Render(summary))
	statusLine := tui.StripControl(m.actionStatus)
	if statusLine == "" {
		statusLine = m.flash
	}
	if statusLine != "" {
		top = append(top, topValDim.Render(" "+statusLine))
	}
	top = append(top, "")

	// Build the left (container list) and right (ports) columns of the body.
	left := m.listLines(listW)
	var right []string
	if panelW > 0 {
		right = m.portsPanelLines(panelW)
	}

	bodyHeight := height - len(top) - 1 // last line is the key bar
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	sep := topValDim.Render(" │ ")
	body := make([]string, bodyHeight)
	for i := 0; i < bodyHeight; i++ {
		l := ""
		if i < len(left) {
			l = left[i]
		}
		l = padOrCrop(l, listW)
		if panelW == 0 {
			body[i] = l
			continue
		}
		r := ""
		if i < len(right) {
			r = right[i]
		}
		body[i] = l + sep + padOrCrop(r, panelW)
	}

	out := append(top, body...)
	return strings.Join(out, "\n") + "\n" + m.topKeyBar(width)
}

// listLines renders the container table (header + rows) at the given width.
func (m topModel) listLines(width int) []string {
	nameW := topNameWidth(width)
	cpuTitle, memTitle := "CPU%", "MEM"
	if m.sortByCPU {
		cpuTitle = "CPU%▾"
	} else {
		memTitle = "MEM▾"
	}
	var lines []string
	header := padOrCrop(topFormatRow("APP", cpuTitle, memTitle, "STATE", nameW), width)
	lines = append(lines, topHeaderBar.Render(header))

	if len(m.rows) == 0 {
		lines = append(lines, topValDim.Render(" Sampling…"))
		return lines
	}
	memTotal := int64(0)
	if m.cur.host != nil {
		memTotal = m.cur.host.GetMemTotalBytes()
	}
	for i, r := range m.rows {
		cpu := "-"
		if r.hasCPU && m.havePrev {
			cpu = fmt.Sprintf("%.1f", r.cpuPercent)
		}
		mem := "—"
		if r.state == agentpb.AppRunningState_RUNNING && memTotal > 0 {
			mem = formatBytes(r.memBytes)
		}
		row := padOrCrop(topFormatRow(topDisplayName(r), cpu, mem, topStateLabel(r.state), nameW), width)
		switch {
		case i == m.cursor:
			lines = append(lines, topSelRow.Render(row))
		case r.state == agentpb.AppRunningState_CRASH_LOOPING:
			lines = append(lines, topCrashRow.Render(row))
		case r.state == agentpb.AppRunningState_STOPPED:
			lines = append(lines, topValDim.Render(row))
		default:
			lines = append(lines, topRunningRow.Render(row))
		}
	}
	return lines
}

// portsPanelLines renders the open-ports panel for the selected app.
func (m topModel) portsPanelLines(width int) []string {
	app := m.selectedAppName()
	appState, hasState := m.selectedAppState()
	title := "OPEN PORTS"
	if app != "" {
		title = "OPEN PORTS — " + app
	}
	lines := []string{topHeaderBar.Render(padOrCrop(" "+title, width))}

	switch {
	case app == "":
		lines = append(lines, topValDim.Render(" (no app selected)"))
	case hasState && appState == agentpb.AppRunningState_STOPPED:
		lines = append(lines, topValDim.Render(" (app is stopped)"))
	case hasState && appState == agentpb.AppRunningState_CRASH_LOOPING:
		lines = append(lines, topValDim.Render(" (app is crash-looping)"))
	case m.portsErr != nil:
		if errors.Is(m.portsErr, errResourceStatsUnimplemented) || status.Code(m.portsErr) == codes.Unimplemented {
			lines = append(lines, topValDim.Render(" (agent too old)"))
		} else {
			lines = append(lines, topValDim.Render(" (unavailable)"))
		}
	case m.portsApp != app:
		lines = append(lines, topValDim.Render(" …"))
	case len(m.ports) == 0:
		lines = append(lines, topValDim.Render(" (none listening)"))
	default:
		for _, p := range m.ports {
			addr := p.GetAddress()
			if strings.Contains(addr, ":") { // IPv6 → bracket for clarity
				addr = "[" + addr + "]"
			}
			lines = append(lines, fmt.Sprintf(" %-4s %s:%d", p.GetProtocol(), addr, p.GetPort()))
		}
	}
	return lines
}

// padOrCrop pads a plain string with spaces to exactly width, or crops it.
func padOrCrop(s string, width int) string {
	n := lipgloss.Width(s)
	if n < width {
		return s + strings.Repeat(" ", width-n)
	}
	if n > width {
		return tui.CropANSIView(s, 0, width)
	}
	return s
}

func (m topModel) topKeyBar(width int) string {
	segs := []struct{ key, label string }{
		{"↑↓", "Nav"},
		{"m", "Mem"},
		{"c", "CPU"},
		{"enter", "Logs"},
		{"s", "Start"},
		{"x", "Stop"},
		{"p", "Clear cache"},
		{"q", "Quit"},
	}
	var b strings.Builder
	plainLen := 0
	for _, s := range segs {
		b.WriteString(topKeyCap.Render(s.key))
		b.WriteString(topKeyLabel.Render(s.label + " "))
		plainLen += lipgloss.Width(s.key) + lipgloss.Width(s.label) + 1
	}
	if plainLen < width {
		b.WriteString(topKeyLabel.Render(strings.Repeat(" ", width-plainLen)))
	}
	return padOrCrop(b.String(), width)
}

func runTopDashboard(ctx context.Context, conn *grpcclient.AgentConnection, interval time.Duration) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := newTopModel(cctx, conn, interval)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(cctx))
	_, err := p.Run()
	return err
}

// formatThermalZones renders thermal zones in the supplied order as a compact
// one-line summary, e.g. "cpu 49°C  gpu 48°C  soc0 47°C". The agent supplies
// them hottest-first; the live dashboard applies a small ordering hysteresis.
// Zone names are shortened by trimming the conventional "-thermal"/"-therm"
// suffix for readability.
func formatThermalZones(zones []*agentpb.ThermalZone) string {
	parts := make([]string, 0, len(zones))
	for _, z := range zones {
		name := z.GetName()
		name = strings.TrimSuffix(name, "-thermal")
		name = strings.TrimSuffix(name, "-therm")
		parts = append(parts, fmt.Sprintf("%s %.0f°C", name, z.GetTempC()))
	}
	return strings.Join(parts, "  ")
}

const thermalOrderHysteresisC = 1.0

// stabilizeThermalZoneOrder keeps the live temperature list hottest-first
// without allowing insignificant fluctuations to reorder it. Starting with
// the prior displayed order, a sensor only overtakes the one above it after it
// becomes more than thermalOrderHysteresisC hotter. Reversing that move needs
// the same clear lead in the other direction, which prevents oscillation while
// still surfacing a meaningfully hotter sensor promptly.
func stabilizeThermalZoneOrder(previous, current []*agentpb.ThermalZone) []*agentpb.ThermalZone {
	if len(current) == 0 {
		return nil
	}

	indicesByName := make(map[string][]int, len(current))
	for i, zone := range current {
		indicesByName[zone.GetName()] = append(indicesByName[zone.GetName()], i)
	}

	ordered := make([]*agentpb.ThermalZone, 0, len(current))
	used := make([]bool, len(current))
	for _, zone := range previous {
		name := zone.GetName()
		indices := indicesByName[name]
		if len(indices) > 0 {
			index := indices[0]
			indicesByName[name] = indices[1:]
			ordered = append(ordered, current[index])
			used[index] = true
		}
	}
	// ResolveThermal sends new sensors hottest-first, so retaining its order
	// here gives a sensible initial placement before hysteresis is applied.
	for i, zone := range current {
		if !used[i] {
			ordered = append(ordered, zone)
		}
	}

	// Insertion-sort from the previous visual order. Unlike a comparator that
	// embeds hysteresis, this remains deterministic and cannot violate Go's
	// strict-ordering requirement.
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && ordered[j].GetTempC() > ordered[j-1].GetTempC()+thermalOrderHysteresisC; j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}
	return ordered
}

const (
	// These are operational thresholds measured on Woof, not vendor ratings.
	// Keep the source-specific distinction: Go2 motors have a lower warning
	// point than the Jetson and Go2 IMU.
	deviceThermalWarningC = 85.0
	go2MotorWarningC      = 70.0
	thermalNearDeltaC     = 5.0
)

type thermalRisk uint8

const (
	thermalNormal thermalRisk = iota
	thermalNear
	thermalOver
)

type thermalReading struct {
	Name  string
	TempC float64
}

type temperatureSummary struct {
	Max            thermalReading
	Risk           thermalRisk
	Alert          thermalReading
	AlertThreshold float64
}

// summarizeTemperature finds the maximum temperature while separately
// classifying every reading against its own operational threshold. This is
// important on a Go2: a 66C motor is near its 70C warning point even when a
// hotter 79C IMU is still more than 5C below its 85C warning point.
func summarizeTemperature(host *agentpb.HostStats) (temperatureSummary, bool) {
	var readings []thermalReading
	for _, zone := range host.GetThermalZones() {
		if zone.GetTempC() > 0 {
			readings = append(readings, thermalReading{Name: zone.GetName(), TempC: zone.GetTempC()})
		}
	}
	for _, gpu := range host.GetGpus() {
		if gpu.TempC != nil && gpu.GetTempC() > 0 {
			readings = append(readings, thermalReading{
				Name:  fmt.Sprintf("gpu/%d", gpu.GetIndex()),
				TempC: gpu.GetTempC(),
			})
		}
	}
	if len(readings) == 0 {
		return temperatureSummary{}, false
	}

	summary := temperatureSummary{Max: readings[0]}
	bestMargin := thermalNearDeltaC + 1
	for _, reading := range readings {
		if reading.TempC > summary.Max.TempC {
			summary.Max = reading
		}
		threshold, classified := thermalWarningThreshold(reading.Name)
		if !classified {
			continue
		}
		margin := threshold - reading.TempC
		risk := thermalNormal
		switch {
		case margin <= 0:
			risk = thermalOver
		case margin <= thermalNearDeltaC:
			risk = thermalNear
		}
		if risk > summary.Risk || (risk == summary.Risk && risk != thermalNormal && margin < bestMargin) {
			summary.Risk = risk
			summary.Alert = reading
			summary.AlertThreshold = threshold
			bestMargin = margin
		}
	}
	return summary, true
}

// thermalWarningThreshold returns an applicable operational warning point.
// Names under go2/ are emitted by the LowState decoder and therefore form a
// stable typed seam rather than a heuristic over arbitrary sysfs labels.
func thermalWarningThreshold(name string) (float64, bool) {
	name = strings.ToLower(name)
	switch {
	case strings.HasPrefix(name, "go2/motor/"):
		return go2MotorWarningC, true
	case name == "go2/imu":
		return deviceThermalWarningC, true
	case strings.HasPrefix(name, "go2/"):
		return 0, false
	default:
		return deviceThermalWarningC, true
	}
}

func formatTemperatureName(name string) string {
	if name == "go2/imu" {
		return "Go2 IMU"
	}
	if motor, ok := strings.CutPrefix(name, "go2/motor/"); ok {
		return "Go2 " + strings.ReplaceAll(motor, "-", " ")
	}
	return strings.TrimSuffix(strings.TrimSuffix(name, "-thermal"), "-therm")
}

func formatTemperatureSummary(summary temperatureSummary) string {
	value := fmt.Sprintf("%.0f°C (%s)", summary.Max.TempC, formatTemperatureName(summary.Max.Name))
	if summary.Risk == thermalNormal {
		return value
	}
	alert := formatTemperatureName(summary.Alert.Name)
	if summary.Alert.Name == summary.Max.Name {
		return fmt.Sprintf("%s, %s %.0f°C warning", value, thermalRiskLabel(summary.Risk), summary.AlertThreshold)
	}
	return fmt.Sprintf("%s — %s %.0f°C, %s %.0f°C warning", value, alert, summary.Alert.TempC, thermalRiskLabel(summary.Risk), summary.AlertThreshold)
}

func thermalRiskLabel(risk thermalRisk) string {
	if risk == thermalOver {
		return "at/above"
	}
	return "near"
}

func renderTemperatureHeader(summary temperatureSummary, zones ...*agentpb.ThermalZone) string {
	line := " Temp: max " + formatTemperatureSummary(summary)
	var remaining []*agentpb.ThermalZone
	for _, zone := range zones {
		if zone.GetName() == summary.Max.Name && zone.GetTempC() == summary.Max.TempC {
			continue
		}
		if summary.Risk != thermalNormal && zone.GetName() == summary.Alert.Name && zone.GetTempC() == summary.Alert.TempC {
			continue
		}
		remaining = append(remaining, zone)
	}
	if details := formatThermalZones(remaining); details != "" {
		line += "  ·  " + details
	}
	switch summary.Risk {
	case thermalNear:
		return " " + topThermalNear.Render("●") + line
	case thermalOver:
		return " " + topThermalHot.Render("●") + line
	default:
		return topValDim.Render(line)
	}
}
