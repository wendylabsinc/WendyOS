package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/version"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func shouldFailDeviceSystemInfo(err error) bool {
	return err != nil && status.Code(err) != codes.Unimplemented
}

func buildDeviceInfoJSON(
	versionResp *agentpb.GetAgentVersionResponse,
	systemInfo *agentpb.GetSystemInfoResponse,
	systemErr error,
	latestVersion string,
	checkUpdates bool,
) map[string]any {
	out := map[string]any{
		"version":         versionResp.GetVersion(),
		"os":              versionResp.GetOs(),
		"osVersion":       versionResp.GetOsVersion(),
		"cpuArchitecture": versionResp.GetCpuArchitecture(),
		"deviceType":      versionResp.GetDeviceType(),
		"cliVersion":      version.Version,
		"hasGpu":          versionResp.GetHasGpu(),
	}
	if sm := versionResp.GetStorageMedium(); sm != "" {
		out["storageMedium"] = sm
	}
	if v := versionResp.GetGpuVendor(); v != "" {
		out["gpuVendor"] = v
	}
	if jv := versionResp.GetJetpackVersion(); jv != "" {
		out["jetpackVersion"] = jv
	}
	if cv := versionResp.GetCudaVersion(); cv != "" {
		out["cudaVersion"] = cv
	}
	if checkUpdates {
		out["latestVersion"] = latestVersion
		out["updateAvailable"] = version.CompareVersions(latestVersion, versionResp.GetVersion()) > 0
	}
	if systemInfo != nil {
		out["collectedAtUnixSeconds"] = systemInfo.GetCollectedAtUnixSeconds()
		out["cpu"] = cpuInfoJSON(systemInfo.GetCpu())
		out["memory"] = memoryInfoJSON(systemInfo.GetMemory())
		out["disks"] = diskInfoJSON(systemInfo.GetDisks())
	} else if systemErr != nil {
		out["systemInfoError"] = deviceSystemInfoErrorMessage(systemErr)
	}
	return out
}

func cpuInfoJSON(cpu *agentpb.GetSystemInfoResponse_CPUInfo) map[string]any {
	if cpu == nil {
		return nil
	}
	out := map[string]any{
		"architecture": cpu.GetArchitecture(),
		"logicalCores": cpu.GetLogicalCores(),
	}
	if cpu.ModelName != nil {
		out["modelName"] = cpu.GetModelName()
	}
	if cpu.UsagePercent != nil {
		out["usagePercent"] = cpu.GetUsagePercent()
	}
	if cpu.LoadAverage_1M != nil {
		out["loadAverage1m"] = cpu.GetLoadAverage_1M()
	}
	if cpu.LoadAverage_5M != nil {
		out["loadAverage5m"] = cpu.GetLoadAverage_5M()
	}
	if cpu.LoadAverage_15M != nil {
		out["loadAverage15m"] = cpu.GetLoadAverage_15M()
	}
	return out
}

func memoryInfoJSON(memory *agentpb.GetSystemInfoResponse_MemoryInfo) map[string]any {
	if memory == nil {
		return nil
	}
	return map[string]any{
		"totalBytes":     memory.GetTotalBytes(),
		"usedBytes":      memory.GetUsedBytes(),
		"availableBytes": memory.GetAvailableBytes(),
		"usedPercent":    memory.GetUsedPercent(),
	}
}

func diskInfoJSON(disks []*agentpb.GetSystemInfoResponse_DiskInfo) []map[string]any {
	out := make([]map[string]any, 0, len(disks))
	for _, disk := range disks {
		out = append(out, map[string]any{
			"mountPoint":     disk.GetMountPoint(),
			"source":         disk.GetSource(),
			"filesystemType": disk.GetFilesystemType(),
			"totalBytes":     disk.GetTotalBytes(),
			"usedBytes":      disk.GetUsedBytes(),
			"freeBytes":      disk.GetFreeBytes(),
			"availableBytes": disk.GetAvailableBytes(),
			"usedPercent":    disk.GetUsedPercent(),
		})
	}
	return out
}

func printDeviceInfoText(
	versionResp *agentpb.GetAgentVersionResponse,
	systemInfo *agentpb.GetSystemInfoResponse,
	systemErr error,
	latestVersion string,
	checkUpdates bool,
) {
	agentVersion := versionResp.GetVersion()
	fmt.Printf("Agent Version: %s\n", agentVersion)
	fmt.Printf("OS: %s %s\n", versionResp.GetOs(), versionResp.GetOsVersion())
	fmt.Printf("Architecture: %s\n", versionResp.GetCpuArchitecture())
	if dt := versionResp.GetDeviceType(); dt != "" {
		fmt.Printf("Device Type: %s\n", dt)
	}
	if sm := versionResp.GetStorageMedium(); sm != "" {
		fmt.Printf("Storage: %s\n", sm)
	}
	if versionResp.GetHasGpu() {
		vendor := versionResp.GetGpuVendor()
		if vendor == "" {
			vendor = "unknown"
		}
		fmt.Printf("GPU: %s\n", vendor)
		if jv := versionResp.GetJetpackVersion(); jv != "" {
			fmt.Printf("JetPack: %s\n", jv)
		}
		if cv := versionResp.GetCudaVersion(); cv != "" {
			fmt.Printf("CUDA: %s\n", cv)
		}
	}
	fmt.Printf("CLI Version: %s\n", version.Version)

	if systemInfo != nil {
		printSystemInfoText(systemInfo)
	} else if systemErr != nil {
		fmt.Printf("\nSystem Info: %s\n", deviceSystemInfoErrorMessage(systemErr))
	}

	if cmp := version.CompareVersions(version.Version, agentVersion); cmp > 0 {
		fmt.Println("\nNote: Agent is behind the CLI. Consider running 'wendy device update'.")
	} else if cmp < 0 {
		fmt.Println("\nNote: CLI is behind the agent. Consider updating the CLI.")
	}

	if checkUpdates {
		if version.CompareVersions(latestVersion, agentVersion) > 0 {
			fmt.Printf("\nUpdate available: %s (you have %s)\nUpdate with: wendy device update\n", latestVersion, agentVersion)
		} else {
			fmt.Println("\nAgent is up to date.")
		}
	}
}

func printSystemInfoText(systemInfo *agentpb.GetSystemInfoResponse) {
	cpu := systemInfo.GetCpu()
	if cpu != nil {
		fmt.Printf("\nCPU: %s", cpu.GetArchitecture())
		if model := cpu.GetModelName(); model != "" {
			fmt.Printf(" - %s", model)
		}
		if cores := cpu.GetLogicalCores(); cores > 0 {
			fmt.Printf(" (%d logical cores)", cores)
		}
		fmt.Println()
		if cpu.UsagePercent != nil {
			fmt.Printf("CPU Usage: %.1f%%\n", cpu.GetUsagePercent())
		}
		if cpu.LoadAverage_1M != nil && cpu.LoadAverage_5M != nil && cpu.LoadAverage_15M != nil {
			fmt.Printf("Load Average: %.2f %.2f %.2f\n", cpu.GetLoadAverage_1M(), cpu.GetLoadAverage_5M(), cpu.GetLoadAverage_15M())
		}
	}

	memory := systemInfo.GetMemory()
	if memory != nil && memory.GetTotalBytes() > 0 {
		fmt.Printf("RAM: %s / %s used (%.1f%%, %s available)\n",
			formatDeviceBytes(memory.GetUsedBytes()),
			formatDeviceBytes(memory.GetTotalBytes()),
			memory.GetUsedPercent(),
			formatDeviceBytes(memory.GetAvailableBytes()),
		)
	}

	disks := systemInfo.GetDisks()
	if len(disks) == 0 {
		return
	}
	rows := make([][]string, 0, len(disks))
	for _, disk := range disks {
		rows = append(rows, []string{
			disk.GetMountPoint(),
			disk.GetSource(),
			disk.GetFilesystemType(),
			formatDeviceBytes(disk.GetUsedBytes()),
			formatDeviceBytes(disk.GetTotalBytes()),
			formatDeviceBytes(disk.GetAvailableBytes()),
			fmt.Sprintf("%.1f%%", disk.GetUsedPercent()),
		})
	}
	fmt.Println("\nDisks:")
	fmt.Print(tui.RenderTable([]string{"Mount", "Source", "FS", "Used", "Total", "Avail", "Use"}, rows))
}

type deviceInfoSystemMsg struct {
	info *agentpb.GetSystemInfoResponse
	err  error
}

type deviceInfoTickMsg struct{}

type deviceInfoModel struct {
	conn          *grpcclient.AgentConnection
	ctx           context.Context
	versionResp   *agentpb.GetAgentVersionResponse
	systemInfo    *agentpb.GetSystemInfoResponse
	systemErr     error
	latestVersion string
	checkUpdates  bool
	width         int
	height        int
	lastRefresh   time.Time
}

func newDeviceInfoModel(
	conn *grpcclient.AgentConnection,
	ctx context.Context,
	versionResp *agentpb.GetAgentVersionResponse,
	systemInfo *agentpb.GetSystemInfoResponse,
	systemErr error,
	latestVersion string,
	checkUpdates bool,
) deviceInfoModel {
	lastRefresh := time.Time{}
	if systemInfo != nil && systemInfo.GetCollectedAtUnixSeconds() > 0 {
		lastRefresh = time.Unix(systemInfo.GetCollectedAtUnixSeconds(), 0)
	}
	return deviceInfoModel{
		conn:          conn,
		ctx:           ctx,
		versionResp:   versionResp,
		systemInfo:    systemInfo,
		systemErr:     systemErr,
		latestVersion: latestVersion,
		checkUpdates:  checkUpdates,
		lastRefresh:   lastRefresh,
	}
}

func (m deviceInfoModel) Init() tea.Cmd {
	return deviceInfoTick()
}

func (m deviceInfoModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "r":
			return m, fetchDeviceSystemInfoCmd(m.conn, m.ctx)
		}
	case deviceInfoTickMsg:
		return m, tea.Batch(fetchDeviceSystemInfoCmd(m.conn, m.ctx), deviceInfoTick())
	case deviceInfoSystemMsg:
		m.systemInfo = msg.info
		m.systemErr = msg.err
		if msg.info != nil && msg.info.GetCollectedAtUnixSeconds() > 0 {
			m.lastRefresh = time.Unix(msg.info.GetCollectedAtUnixSeconds(), 0)
		} else if msg.err == nil {
			m.lastRefresh = time.Now()
		}
		return m, nil
	}
	return m, nil
}

func (m deviceInfoModel) View() string {
	var sb strings.Builder
	sb.WriteString(deviceInfoTitleStyle.Render("Wendy Device Info") + "\n\n")
	sb.WriteString(m.agentSection())
	sb.WriteString("\n")
	sb.WriteString(m.systemSection())
	sb.WriteString("\n")
	sb.WriteString(m.diskSection())
	sb.WriteString("\n")
	sb.WriteString(deviceInfoFooterStyle.Render(m.footer()))
	return sb.String()
}

func (m deviceInfoModel) agentSection() string {
	rows := [][]string{
		{"Agent Version", m.versionResp.GetVersion()},
		{"CLI Version", version.Version},
		{"OS", strings.TrimSpace(m.versionResp.GetOs() + " " + m.versionResp.GetOsVersion())},
		{"Architecture", m.versionResp.GetCpuArchitecture()},
	}
	if dt := m.versionResp.GetDeviceType(); dt != "" {
		rows = append(rows, []string{"Device Type", dt})
	}
	if sm := m.versionResp.GetStorageMedium(); sm != "" {
		rows = append(rows, []string{"Storage", sm})
	}
	if m.versionResp.GetHasGpu() {
		gpu := m.versionResp.GetGpuVendor()
		if gpu == "" {
			gpu = "unknown"
		}
		rows = append(rows, []string{"GPU", gpu})
	}
	if m.checkUpdates {
		if version.CompareVersions(m.latestVersion, m.versionResp.GetVersion()) > 0 {
			rows = append(rows, []string{"Update", fmt.Sprintf("%s available", m.latestVersion)})
		} else {
			rows = append(rows, []string{"Update", "agent is up to date"})
		}
	}
	return tui.RenderTable([]string{"Field", "Value"}, rows)
}

func (m deviceInfoModel) systemSection() string {
	if m.systemErr != nil && m.systemInfo == nil {
		return deviceInfoErrorStyle.Render("System Info: "+deviceSystemInfoErrorMessage(m.systemErr)) + "\n"
	}
	if m.systemInfo == nil {
		return deviceInfoDimStyle.Render("System Info: waiting for agent response") + "\n"
	}

	rows := make([][]string, 0, 4)
	cpu := m.systemInfo.GetCpu()
	if cpu != nil {
		cpuValue := cpu.GetArchitecture()
		if cpu.GetModelName() != "" {
			cpuValue = cpu.GetModelName() + " (" + cpu.GetArchitecture() + ")"
		}
		if cpu.GetLogicalCores() > 0 {
			cpuValue += fmt.Sprintf(", %d logical cores", cpu.GetLogicalCores())
		}
		rows = append(rows, []string{"CPU", cpuValue})
		if cpu.UsagePercent != nil {
			rows = append(rows, []string{"CPU Usage", fmt.Sprintf("%.1f%%", cpu.GetUsagePercent())})
		}
		if cpu.LoadAverage_1M != nil && cpu.LoadAverage_5M != nil && cpu.LoadAverage_15M != nil {
			rows = append(rows, []string{"Load Avg", fmt.Sprintf("%.2f %.2f %.2f", cpu.GetLoadAverage_1M(), cpu.GetLoadAverage_5M(), cpu.GetLoadAverage_15M())})
		}
	}
	if memory := m.systemInfo.GetMemory(); memory != nil && memory.GetTotalBytes() > 0 {
		rows = append(rows, []string{"RAM", fmt.Sprintf("%s / %s used (%.1f%%, %s available)",
			formatDeviceBytes(memory.GetUsedBytes()),
			formatDeviceBytes(memory.GetTotalBytes()),
			memory.GetUsedPercent(),
			formatDeviceBytes(memory.GetAvailableBytes()),
		)})
	}
	if len(rows) == 0 {
		return deviceInfoDimStyle.Render("System Info: no CPU or memory data reported") + "\n"
	}
	return tui.RenderTable([]string{"Resource", "Value"}, rows)
}

func (m deviceInfoModel) diskSection() string {
	if m.systemInfo == nil || len(m.systemInfo.GetDisks()) == 0 {
		return deviceInfoDimStyle.Render("Disks: no mounted disk data reported") + "\n"
	}

	rows := make([][]string, 0, len(m.systemInfo.GetDisks()))
	for _, disk := range m.systemInfo.GetDisks() {
		rows = append(rows, []string{
			disk.GetMountPoint(),
			disk.GetSource(),
			disk.GetFilesystemType(),
			formatDeviceBytes(disk.GetUsedBytes()),
			formatDeviceBytes(disk.GetTotalBytes()),
			formatDeviceBytes(disk.GetAvailableBytes()),
			fmt.Sprintf("%.1f%%", disk.GetUsedPercent()),
		})
	}
	return tui.RenderTable([]string{"Mount", "Source", "FS", "Used", "Total", "Avail", "Use"}, rows)
}

func (m deviceInfoModel) footer() string {
	parts := []string{"r refresh", "q quit"}
	if !m.lastRefresh.IsZero() {
		parts = append(parts, "updated "+m.lastRefresh.Format("15:04:05"))
	}
	return strings.Join(parts, " | ")
}

func fetchDeviceSystemInfoCmd(conn *grpcclient.AgentConnection, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		info, err := conn.AgentService.GetSystemInfo(ctx, &agentpb.GetSystemInfoRequest{})
		return deviceInfoSystemMsg{info: info, err: err}
	}
}

func deviceInfoTick() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg {
		return deviceInfoTickMsg{}
	})
}

func deviceSystemInfoErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	if status.Code(err) == codes.Unimplemented {
		return "agent does not support system info; run 'wendy device update'"
	}
	return err.Error()
}

func formatDeviceBytes(n uint64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1f GB", float64(n)/1_000_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1f MB", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1f kB", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

var (
	deviceInfoTitleStyle  = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary)
	deviceInfoDimStyle    = lipgloss.NewStyle().Foreground(tui.ColorDim)
	deviceInfoErrorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	deviceInfoFooterStyle = lipgloss.NewStyle().Foreground(tui.ColorDim)
)
