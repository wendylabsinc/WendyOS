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

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

var (
	// Styled icons for static (non-interactive) table output.
	stateIconRunning   = lipgloss.NewStyle().Foreground(tui.Emerald400).Render("●")
	stateIconStopped   = lipgloss.NewStyle().Foreground(tui.ColorDim).Render("●")
	stateIconCrashLoop = lipgloss.NewStyle().Foreground(tui.Red500).Render("↻")
)

func newAppsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apps",
		Short: "Manage applications on the target device",
	}

	cmd.AddCommand(
		newAppsListCmd(),
		newAppsInstallCmd(),
		newAppsStartCmd(),
		newAppsStopCmd(),
		newAppsRemoveCmd(),
	)

	return cmd
}

func newAppsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List deployed applications",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target, err := resolveTarget(ctx, IncludeBluetooth())
			if err != nil {
				return err
			}
			defer target.Close()

			if target.Bluetooth != nil && target.Bluetooth.IsWendyAgent() {
				cliLogln("Connecting to %s via Bluetooth...", target.Bluetooth.DisplayName)
				apps, listErr := listApps(ctx, target)
				if listErr != nil {
					return listErr
				}
				if jsonOutput {
					type jsonApp struct {
						Name    string `json:"name"`
						Version string `json:"version,omitempty"`
						State   string `json:"state,omitempty"`
					}
					out := make([]jsonApp, len(apps))
					for i, a := range apps {
						out[i] = jsonApp{Name: a.Name, Version: a.Version, State: a.State}
					}
					data, jsonErr := json.MarshalIndent(out, "", "  ")
					if jsonErr != nil {
						return jsonErr
					}
					fmt.Println(string(data))
					return nil
				}
				if len(apps) == 0 {
					cliLogln("No applications deployed.")
					return nil
				}
				headers := []string{"", "Name", "Version"}
				var rows [][]string
				for _, a := range apps {
					rows = append(rows, []string{stateIcon(a.State), a.Name, a.Version})
				}
				fmt.Print(tui.RenderTable(headers, rows))
				return nil
			}
			if target.Agent != nil {
				return appsListAgent(ctx, target.Agent)
			}
			if target.Provider != nil {
				cm, ok := target.Provider.(providers.ContainerManager)
				if !ok {
					return fmt.Errorf("selected device does not support container management")
				}
				return appsListProvider(ctx, cm)
			}
			return fmt.Errorf("selected device does not support this command")
		},
	}
}

func appsListAgent(ctx context.Context, conn *grpcclient.AgentConnection) error {
	// Interactive dashboard when stdin/stdout are interactive and --json is not set.
	if !jsonOutput && isInteractiveTerminal() {
		return runAppsDashboard(ctx, conn)
	}

	// Static / JSON path (unchanged).
	stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}

	var containers []*agentpb.AppContainer
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("receiving container list: %w", err)
		}
		if c := resp.GetContainer(); c != nil {
			containers = append(containers, c)
		}
	}
	sortRunningFirst(containers, func(c *agentpb.AppContainer) string {
		return c.GetRunningState().String()
	})

	if jsonOutput {
		type jsonService struct {
			Name         string `json:"name"`
			RunningState string `json:"runningState"`
		}
		type jsonApp struct {
			Name              string        `json:"name"`
			Version           string        `json:"version,omitempty"`
			RunningState      string        `json:"runningState,omitempty"`
			FailureCount      uint32        `json:"failureCount,omitempty"`
			ExitCode          *int32        `json:"exitCode,omitempty"` // pointer so a clean exit 0 is still emitted alongside terminationReason
			TerminationReason string        `json:"terminationReason,omitempty"`
			HTTPPort          uint32        `json:"httpPort,omitempty"`
			Services          []jsonService `json:"services,omitempty"`
		}
		apps := make([]jsonApp, len(containers))
		for i, c := range containers {
			var svcs []jsonService
			for _, s := range c.GetServices() {
				svcs = append(svcs, jsonService{
					Name:         s.GetName(),
					RunningState: s.GetRunningState().String(),
				})
			}
			var exitCode *int32
			if c.GetTerminationReason() != "" {
				ec := c.GetExitCode()
				exitCode = &ec
			}
			apps[i] = jsonApp{
				Name:              c.GetAppName(),
				Version:           c.GetAppVersion(),
				RunningState:      c.GetRunningState().String(),
				FailureCount:      c.GetFailureCount(),
				ExitCode:          exitCode,
				TerminationReason: c.GetTerminationReason(),
				HTTPPort:          c.GetHttpPort(),
				Services:          svcs,
			}
		}
		data, err := json.MarshalIndent(apps, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	if len(containers) == 0 {
		cliLogln("No applications deployed.")
		return nil
	}
	headers := []string{"", "Name", "Version", "Port", "Failures", "Reason"}
	var rows [][]string
	for _, c := range containers {
		services := c.GetServices()
		if len(services) > 1 {
			// Group header row: aggregate state + app name marked as group.
			rows = append(rows, []string{
				stateIcon(c.GetRunningState().String()),
				c.GetAppName() + " " + lipgloss.NewStyle().Foreground(tui.ColorDim).Render("[group]"),
				c.GetAppVersion(),
				httpPortColumn(c.GetHttpPort()),
				fmt.Sprintf("%d", c.GetFailureCount()),
				terminationSummary(c.GetTerminationReason(), c.GetExitCode()),
			})
			// Per-service sub-rows indented under the group header.
			for _, s := range services {
				rows = append(rows, []string{
					"",
					"  ↳ " + s.GetName() + "  " + stateIcon(s.GetRunningState().String()),
					"",
					"",
					"",
					"",
				})
			}
		} else {
			rows = append(rows, []string{
				stateIcon(c.GetRunningState().String()),
				c.GetAppName(),
				c.GetAppVersion(),
				httpPortColumn(c.GetHttpPort()),
				fmt.Sprintf("%d", c.GetFailureCount()),
				terminationSummary(c.GetTerminationReason(), c.GetExitCode()),
			})
		}
	}
	fmt.Print(tui.RenderTable(headers, rows))
	return nil
}

// httpPortColumn renders an app's declared http entitlement port for the
// apps-list table, or "" when the app declares none.
func httpPortColumn(port uint32) string {
	if port == 0 {
		return ""
	}
	return fmt.Sprintf(":%d", port)
}

// terminationSummary renders a stopped app's exit reason for display. Empty for
// a running app or one whose cause wasn't recorded. Mirrors the agent's
// termination_reason vocabulary (see exit_status.go).
func terminationSummary(reason string, exitCode int32) string {
	switch reason {
	case "":
		return ""
	case "crashed":
		return fmt.Sprintf("crashed (exit %d)", exitCode)
	case "oom_killed":
		return "OOM killed"
	case "start_failed":
		return "start failed"
	case "entitlement_denied":
		return "entitlement denied"
	case "device_unavailable":
		// Deliberately says where to look. This reason exists because the same
		// failure used to surface as "crashed (exit 139)", which sends people
		// into the app's code instead of at the device it asked for.
		return "device unavailable on the host (see 'wendy device hardware list')"
	case "exited":
		return "exited"
	default:
		return reason
	}
}

// streamAppLogs streams logs for a single app to stdout after the dashboard exits.
func streamAppLogs(ctx context.Context, conn *grpcclient.AgentConnection, appName string) error {
	cliLogln("Streaming logs for %s (Ctrl+C to stop)…", appName)
	req := &agentpb.StreamLogsRequest{AppName: &appName}
	stream, err := conn.TelemetryService.StreamLogs(ctx, req)
	if err != nil {
		return fmt.Errorf("starting log stream: %w", err)
	}
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("receiving logs: %w", err)
		}
		logs := resp.GetLogs()
		if logs == nil {
			continue
		}
		for _, rl := range logs.GetResourceLogs() {
			svc := resourceServiceName(rl.GetResource())
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					printLogRecord(svc, lr)
				}
			}
		}
	}
}

func runAppsDashboard(ctx context.Context, conn *grpcclient.AgentConnection) error {
	// Use a cancellable context so background goroutines stop when the program exits.
	dashCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	m := newAppsDashboardModel(conn, dashCtx)

	// Wire up the 'd' key to set the current device as default (same pattern as picker).
	m.OnSetDefault = func() {
		if cfg, err := config.Load(); err == nil {
			cfg.DefaultDevice = conn.Host
			_ = config.Save(cfg)
		}
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	result, err := p.Run()
	if err != nil {
		return fmt.Errorf("apps dashboard: %w", err)
	}

	final, ok := result.(appsDashboardModel)
	if !ok {
		return nil
	}

	if final.action == appsDashActionLogs && final.selectedApp != "" {
		return streamAppLogs(ctx, conn, final.selectedApp)
	}
	return nil
}

func appsListProvider(ctx context.Context, cm providers.ContainerManager) error {
	containers, err := cm.ListContainers(ctx)
	if err != nil {
		return err
	}
	sortRunningFirst(containers, func(c providers.ContainerInfo) string {
		return c.State
	})

	if jsonOutput {
		data, err := json.MarshalIndent(containers, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	if len(containers) == 0 {
		cliLogln("No applications deployed.")
		return nil
	}

	headers := []string{"", "Name", "Image", "Status"}
	var rows [][]string
	for _, c := range containers {
		rows = append(rows, []string{stateIcon(c.State), c.Name, c.Image, c.Status})
	}
	fmt.Print(tui.RenderTable(headers, rows))
	return nil
}

func newAppsStartCmd() *cobra.Command {
	var detach bool

	cmd := &cobra.Command{
		Use:   "start [app-name]",
		Short: "Start an application",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target, err := resolveTarget(ctx)
			if err != nil {
				return err
			}
			defer target.Close()

			var appName string
			if len(args) > 0 {
				appName = args[0]
			} else {
				appName, err = pickApp(ctx, target, "Select an app to start")
				if err != nil {
					return err
				}
			}

			if target.Agent != nil {
				if detach {
					stream, err := target.Agent.ContainerService.StartContainer(ctx, &agentpb.StartContainerRequest{
						AppName:       appName,
						RestartPolicy: &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED},
					})
					if err != nil {
						return fmt.Errorf("starting container: %w", err)
					}
					// Wait for the agent's Started confirmation before returning.
					// Returning immediately (the old behavior) closed the connection
					// and canceled the RPC before the agent had loaded the container,
					// so the start silently failed while we reported success.
					if err := awaitStarted(stream); err != nil {
						return fmt.Errorf("starting container: %w", err)
					}
					cliSuccess("Application %s started.", appName)
					return nil
				}
				outStream, stdinAttempted, err := openContainerStream(ctx, target.Agent.ContainerService, appName, nil)
				if err != nil {
					return err
				}
				gotFirstResponse := false
				gotStarted := false
				for {
					resp, err := outStream.Recv()
					if err == io.EOF {
						break
					}
					if err != nil {
						if stdinAttempted && !gotFirstResponse && status.Code(err) == codes.Unimplemented {
							cliNotice("Notice: stdin not attached (not supported by agent)")
							startStream, startErr := target.Agent.ContainerService.StartContainer(ctx, &agentpb.StartContainerRequest{
								AppName: appName,
							})
							if startErr != nil {
								return fmt.Errorf("starting container: %w", startErr)
							}
							outStream = startStream
							stdinAttempted = false
							continue
						}
						return fmt.Errorf("receiving start response: %w", err)
					}
					gotFirstResponse = true
					if resp.GetStarted() != nil {
						gotStarted = true
					}
					if out := resp.GetStdoutOutput(); out != nil {
						os.Stdout.Write(out.GetData())
					}
					if out := resp.GetStderrOutput(); out != nil {
						os.Stderr.Write(out.GetData())
					}
				}
				// The agent never confirmed the start: the container did not run.
				if !gotStarted {
					return fmt.Errorf("container %q did not start", appName)
				}
				// The stream ended. A single container's stream ends when it
				// exits; a group start's stream ends immediately while the
				// services keep running. Report the actual state instead of
				// guessing from whether any output was seen.
				reportStartOutcome(ctx, target.Agent.ContainerService, appName)
				return nil
			}

			if target.Provider != nil {
				cm, ok := target.Provider.(providers.ContainerManager)
				if !ok {
					return fmt.Errorf("selected device does not support container management")
				}
				if err := cm.StartContainer(ctx, appName); err != nil {
					return err
				}
				cliSuccess("Application %s started.", appName)
				return nil
			}

			return fmt.Errorf("selected device does not support this command")
		},
	}

	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "Start without streaming output")
	return cmd
}

// awaitStarted consumes a start/run response stream until the agent sends its
// Started marker (returning nil). It returns an error if the stream fails or
// closes before Started arrives — which means the container never started.
// Interleaved output frames before Started are discarded (detached callers do
// not stream output).
func awaitStarted(stream containerOutputStream) error {
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return fmt.Errorf("agent closed the stream before confirming the container started")
		}
		if err != nil {
			return err
		}
		if resp.GetStarted() != nil {
			return nil
		}
	}
}

// reportStartOutcome prints the container's actual post-start state after a
// non-detached start stream ends, so the message reflects reality rather than
// guessing from streamed output. RUNNING (a group start, or a container still
// up) reads as started; a terminated single container reports how it ended.
// A failure state (crash, crash-loop) is a neutral notice, not success styling;
// the exit status stays 0 because the start itself was confirmed.
func reportStartOutcome(ctx context.Context, svc agentpb.WendyContainerServiceClient, appName string) {
	c := fetchAppContainer(ctx, svc, appName)
	if c == nil {
		cliSuccess("Application %s started.", appName)
		return
	}
	switch c.GetRunningState() {
	case agentpb.AppRunningState_RUNNING:
		cliSuccess("Application %s started.", appName)
	case agentpb.AppRunningState_CRASH_LOOPING:
		cliNotice("Application %s is crash-looping (%s).", appName,
			terminationSummary(c.GetTerminationReason(), c.GetExitCode()))
	default: // STOPPED
		switch c.GetTerminationReason() {
		case "", "exited":
			if summary := terminationSummary(c.GetTerminationReason(), c.GetExitCode()); summary != "" {
				cliSuccess("Application %s %s.", appName, summary)
			} else {
				cliSuccess("Application %s stopped.", appName)
			}
		default: // crashed / oom_killed / start_failed / entitlement_denied / device_unavailable
			cliNotice("Application %s %s.", appName,
				terminationSummary(c.GetTerminationReason(), c.GetExitCode()))
		}
	}
}

// fetchAppContainer returns the AppContainer for appName from the agent's
// container list, or nil if it cannot be read or found.
func fetchAppContainer(ctx context.Context, svc agentpb.WendyContainerServiceClient, appName string) *agentpb.AppContainer {
	stream, err := svc.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		return nil
	}
	var found *agentpb.AppContainer
	for {
		resp, err := stream.Recv()
		if err != nil {
			break
		}
		if c := resp.GetContainer(); c != nil && c.GetAppName() == appName {
			found = c
		}
	}
	return found
}

func newAppsStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop [app-name]",
		Short: "Stop an application",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target, err := resolveTarget(ctx, IncludeBluetooth())
			if err != nil {
				return err
			}
			defer target.Close()

			var appName string
			if len(args) > 0 {
				appName = args[0]
			} else {
				appName, err = pickApp(ctx, target, "Select an app to stop")
				if err != nil {
					return err
				}
			}

			if target.Bluetooth != nil && target.Bluetooth.IsWendyAgent() {
				cliLogln("Connecting to %s via Bluetooth...", target.Bluetooth.DisplayName)
				bleClient, bleErr := connectBLEAgent(target.Bluetooth)
				if bleErr != nil {
					return bleErr
				}
				defer bleClient.Close()
				if bleErr = bleClient.AppsStop(appName); bleErr != nil {
					return fmt.Errorf("stopping app: %w", bleErr)
				}
				cliSuccess("Application %s stopped.", appName)
				return nil
			}

			if target.Agent != nil {
				_, err = target.Agent.ContainerService.StopContainer(ctx, &agentpb.StopContainerRequest{
					AppName: appName,
				})
				if err != nil {
					return fmt.Errorf("stopping container: %w", err)
				}
				cliSuccess("Application %s stopped.", appName)
				return nil
			}

			if target.Provider != nil {
				cm, ok := target.Provider.(providers.ContainerManager)
				if !ok {
					return fmt.Errorf("selected device does not support container management")
				}
				if err := cm.StopContainer(ctx, appName); err != nil {
					return err
				}
				cliSuccess("Application %s stopped.", appName)
				return nil
			}

			return fmt.Errorf("selected device does not support this command")
		},
	}
}

func newAppsRemoveCmd() *cobra.Command {
	var force bool
	var cleanup bool
	var deleteVolumes bool

	cmd := &cobra.Command{
		Use:   "remove [app-name]",
		Short: "Remove an application",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target, err := resolveTarget(ctx, IncludeBluetooth())
			if err != nil {
				return err
			}
			defer target.Close()

			var appName string
			if len(args) > 0 {
				appName = args[0]
			} else {
				appName, err = pickApp(ctx, target, "Select an app to remove")
				if err != nil {
					return err
				}
			}

			// Confirmation prompt (unless --force).
			if !force {
				confirmed, err := tui.Confirm(fmt.Sprintf("Remove %s? This cannot be undone.", appName))
				if err != nil {
					if errors.Is(err, tui.ErrCancelled) {
						return ErrUserCancelled
					}
					return err
				}
				if !confirmed {
					cliNotice("Cancelled.")
					return nil
				}
			}

			// If neither --cleanup nor --delete-volumes was explicitly set,
			// offer an interactive checklist for cleanup options.
			cleanupSet := cmd.Flags().Changed("cleanup")
			volumesSet := cmd.Flags().Changed("delete-volumes")
			if !cleanupSet && !volumesSet && !force {
				items := []tui.ChecklistItem{
					{Label: "Delete container image", Description: "Frees disk space", Value: "cleanup"},
					{Label: "Delete persistent volumes", Description: "Removes data in /var/lib/wendy/volumes", Value: "volumes"},
				}
				selected, err := tui.RunChecklist("Also clean up?", items)
				if err != nil {
					if errors.Is(err, tui.ErrCancelled) {
						return ErrUserCancelled
					}
					return err
				}
				for _, item := range selected {
					switch item.Value {
					case "cleanup":
						cleanup = true
					case "volumes":
						deleteVolumes = true
					}
				}
			}

			if target.Bluetooth != nil && target.Bluetooth.IsWendyAgent() {
				cliLogln("Connecting to %s via Bluetooth...", target.Bluetooth.DisplayName)
				bleClient, bleErr := connectBLEAgent(target.Bluetooth)
				if bleErr != nil {
					return bleErr
				}
				defer bleClient.Close()
				if bleErr = bleClient.AppsRemove(appName, cleanup); bleErr != nil {
					return fmt.Errorf("removing app: %w", bleErr)
				}
				cliSuccess("Application %s removed.", appName)
				if cleanup {
					cliLogln("  Container image cleanup requested.")
				}
				return nil
			}

			if target.Agent != nil {
				_, err = target.Agent.ContainerService.DeleteContainer(ctx, &agentpb.DeleteContainerRequest{
					AppName:       appName,
					DeleteImage:   cleanup,
					DeleteVolumes: deleteVolumes,
				})
				if err != nil {
					return fmt.Errorf("removing container: %w", err)
				}
				cliSuccess("Application %s removed.", appName)
				if cleanup {
					cliLogln("  Container image cleanup requested.")
				}
				if deleteVolumes {
					cliLogln("  Persistent volume deletion requested.")
				}
				return nil
			}

			if target.Provider != nil {
				cm, ok := target.Provider.(providers.ContainerManager)
				if !ok {
					return fmt.Errorf("selected device does not support container management")
				}
				if err := cm.RemoveContainer(ctx, appName); err != nil {
					return err
				}
				cliSuccess("Application %s removed.", appName)
				return nil
			}

			return fmt.Errorf("selected device does not support this command")
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation prompt")
	cmd.Flags().BoolVar(&cleanup, "cleanup", false, "Also delete the container image (frees disk space; agent-connected devices only)")
	cmd.Flags().BoolVar(&deleteVolumes, "delete-volumes", false, "Also delete persistent volumes (/var/lib/wendy/volumes; agent-connected devices only)")
	return cmd
}

func newPsCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "ps",
		Short:  "List running containers (alias for 'apps list')",
		Hidden: true,
		RunE:   newAppsListCmd().RunE,
	}
}

func stateIcon(state string) string {
	switch strings.ToLower(state) {
	case "running":
		return stateIconRunning
	case "crash_looping":
		return stateIconCrashLoop
	default:
		return stateIconStopped
	}
}

func stateIconPlain(state string) string {
	switch strings.ToLower(state) {
	case "running":
		return "●"
	case "crash_looping":
		return "↻"
	default:
		return "○"
	}
}

type appInfo struct {
	Name    string
	Version string
	State   string
	IsGroup bool // true when this app has multiple service containers
}

// sortRunningFirst performs a stable partition so running apps appear before
// every other state while preserving the device's order within each group.
func sortRunningFirst[T any](items []T, state func(T) string) {
	sort.SliceStable(items, func(i, j int) bool {
		iRunning := strings.EqualFold(state(items[i]), "running")
		jRunning := strings.EqualFold(state(items[j]), "running")
		return iRunning && !jRunning
	})
}

func listApps(ctx context.Context, target *SelectedDevice) ([]appInfo, error) {
	if target.Bluetooth != nil && target.Bluetooth.IsWendyAgent() {
		bleClient, err := connectBLEAgent(target.Bluetooth)
		if err != nil {
			return nil, err
		}
		defer bleClient.Close()
		bleApps, err := bleClient.AppsList()
		if err != nil {
			return nil, fmt.Errorf("listing apps: %w", err)
		}
		apps := make([]appInfo, len(bleApps))
		for i, a := range bleApps {
			apps[i] = appInfo{
				Name:    a.GetAppName(),
				Version: a.GetAppVersion(),
				State:   a.GetState(),
			}
		}
		sortRunningFirst(apps, func(a appInfo) string { return a.State })
		return apps, nil
	}

	if target.Agent != nil {
		stream, err := target.Agent.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
		if err != nil {
			return nil, fmt.Errorf("listing containers: %w", err)
		}
		var apps []appInfo
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("receiving container list: %w", err)
			}
			if c := resp.GetContainer(); c != nil {
				apps = append(apps, appInfo{
					Name:    c.GetAppName(),
					Version: c.GetAppVersion(),
					State:   c.GetRunningState().String(),
					IsGroup: len(c.GetServices()) > 1,
				})
			}
		}
		sortRunningFirst(apps, func(a appInfo) string { return a.State })
		return apps, nil
	}

	if target.Provider != nil {
		cm, ok := target.Provider.(providers.ContainerManager)
		if !ok {
			return nil, fmt.Errorf("selected device does not support container management")
		}
		containers, err := cm.ListContainers(ctx)
		if err != nil {
			return nil, err
		}
		apps := make([]appInfo, len(containers))
		for i, c := range containers {
			apps[i] = appInfo{Name: c.Name, State: c.State}
		}
		sortRunningFirst(apps, func(a appInfo) string { return a.State })
		return apps, nil
	}

	return nil, fmt.Errorf("selected device does not support this command")
}

// pickApp presents an interactive picker for selecting an app from the target
// device. It returns the selected app name or an error.
func pickApp(ctx context.Context, target *SelectedDevice, title string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("no app name specified; run in an interactive terminal or pass the app name as an argument")
	}

	apps, err := listApps(ctx, target)
	if err != nil {
		return "", err
	}
	if len(apps) == 0 {
		return "", fmt.Errorf("no applications found on device")
	}

	picker := tui.NewPickerWithTitle(title)
	p := tea.NewProgram(picker)

	go func() {
		var items []tui.PickerItem
		for _, app := range apps {
			name := stateIconPlain(app.State) + " " + app.Name
			if app.IsGroup {
				name += " [group]"
			}
			items = append(items, tui.PickerItem{
				Name:        name,
				Description: app.Version,
				Value:       app.Name,
			})
		}
		p.Send(tui.PickerAddMsg{Items: items})
		p.Send(tui.PickerDoneMsg{})
	}()

	finalModel, err := p.Run()
	if err != nil {
		return "", fmt.Errorf("app picker: %w", err)
	}

	pm := finalModel.(tui.PickerModel)
	if pm.Cancelled() {
		return "", ErrUserCancelled
	}
	sel := pm.Selected()
	if sel == nil {
		return "", fmt.Errorf("no app selected")
	}

	name, ok := sel.Value.(string)
	if !ok {
		return "", fmt.Errorf("invalid selection")
	}
	return name, nil
}
