package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/providers"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func newAppsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apps",
		Short: "Manage applications on the target device",
	}

	cmd.AddCommand(
		newAppsListCmd(),
		newAppsStartCmd(),
		newAppsStopCmd(),
		newAppsRemoveCmd(),
	)

	return cmd
}

func newAppsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List running applications",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target, err := resolveTarget(ctx)
			if err != nil {
				return err
			}
			defer target.Close()

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

	if jsonOutput {
		type jsonApp struct {
			Name         string `json:"name"`
			Version      string `json:"version,omitempty"`
			RunningState string `json:"runningState,omitempty"`
			FailureCount uint32 `json:"failureCount,omitempty"`
		}

		apps := make([]jsonApp, len(containers))
		for i, c := range containers {
			apps[i] = jsonApp{
				Name:         c.GetAppName(),
				Version:      c.GetAppVersion(),
				RunningState: c.GetRunningState().String(),
				FailureCount: c.GetFailureCount(),
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
		fmt.Println("No applications running.")
		return nil
	}

	headers := []string{"Name", "Version", "State", "Failures"}
	var rows [][]string
	for _, c := range containers {
		rows = append(rows, []string{
			c.GetAppName(),
			c.GetAppVersion(),
			c.GetRunningState().String(),
			fmt.Sprintf("%d", c.GetFailureCount()),
		})
	}
	fmt.Print(tui.RenderTable(headers, rows))
	return nil
}

func appsListProvider(ctx context.Context, cm providers.ContainerManager) error {
	containers, err := cm.ListContainers(ctx)
	if err != nil {
		return err
	}

	if jsonOutput {
		data, err := json.MarshalIndent(containers, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	if len(containers) == 0 {
		fmt.Println("No applications found.")
		return nil
	}

	headers := []string{"Name", "Image", "State", "Status"}
	var rows [][]string
	for _, c := range containers {
		rows = append(rows, []string{c.Name, c.Image, c.State, c.Status})
	}
	fmt.Print(tui.RenderTable(headers, rows))
	return nil
}

func newAppsStartCmd() *cobra.Command {
	return &cobra.Command{
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
				stream, err := target.Agent.ContainerService.StartContainer(ctx, &agentpb.StartContainerRequest{
					AppName: appName,
				})
				if err != nil {
					return fmt.Errorf("starting container: %w", err)
				}
				for {
					resp, err := stream.Recv()
					if err == io.EOF {
						break
					}
					if err != nil {
						return fmt.Errorf("receiving start response: %w", err)
					}
					if out := resp.GetStdoutOutput(); out != nil {
						fmt.Print(string(out.GetData()))
					}
					if out := resp.GetStderrOutput(); out != nil {
						fmt.Print(string(out.GetData()))
					}
				}
				fmt.Printf("Application %s started.\n", appName)
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
				fmt.Printf("Application %s started.\n", appName)
				return nil
			}

			return fmt.Errorf("selected device does not support this command")
		},
	}
}

func newAppsStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop [app-name]",
		Short: "Stop an application",
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
				appName, err = pickApp(ctx, target, "Select an app to stop")
				if err != nil {
					return err
				}
			}

			if target.Agent != nil {
				_, err = target.Agent.ContainerService.StopContainer(ctx, &agentpb.StopContainerRequest{
					AppName: appName,
				})
				if err != nil {
					return fmt.Errorf("stopping container: %w", err)
				}
				fmt.Printf("Application %s stopped.\n", appName)
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
				fmt.Printf("Application %s stopped.\n", appName)
				return nil
			}

			return fmt.Errorf("selected device does not support this command")
		},
	}
}

func newAppsRemoveCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "remove [app-name]",
		Short: "Remove an application",
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
				appName, err = pickApp(ctx, target, "Select an app to remove")
				if err != nil {
					return err
				}
			}

			if !force {
				fmt.Printf("Are you sure you want to remove %s? This cannot be undone. Use --force to skip confirmation.\n", appName)
				return nil
			}

			if target.Agent != nil {
				_, err = target.Agent.ContainerService.DeleteContainer(ctx, &agentpb.DeleteContainerRequest{
					AppName: appName,
				})
				if err != nil {
					return fmt.Errorf("removing container: %w", err)
				}
				fmt.Printf("Application %s removed.\n", appName)
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
				fmt.Printf("Application %s removed.\n", appName)
				return nil
			}

			return fmt.Errorf("selected device does not support this command")
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation prompt")
	return cmd
}

// appInfo holds the display information for an app returned by the agent or provider.
type appInfo struct {
	Name    string
	Version string
	State   string
}

// listApps fetches the list of apps from the target device.
func listApps(ctx context.Context, target *SelectedDevice) ([]appInfo, error) {
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
				})
			}
		}
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
			desc := app.State
			if app.Version != "" {
				desc = app.Version + " · " + desc
			}
			items = append(items, tui.PickerItem{
				Name:        app.Name,
				Description: desc,
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
