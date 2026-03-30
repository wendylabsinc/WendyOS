package commands

import (
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func newOSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "os",
		Short: "Manage the WendyOS operating system",
	}

	cmd.AddCommand(newOSUpdateCmd())
	cmd.AddCommand(newOSListDrivesCmd())
	addOSInstallCmd(cmd)
	addOSDownloadCmd(cmd)
	addOSCacheCmd(cmd)
	return cmd
}

func newOSUpdateCmd() *cobra.Command {
	var artifactURL string

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update WendyOS on the target device",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			stream, err := conn.AgentService.UpdateOS(ctx, &agentpb.UpdateOSRequest{
				ArtifactUrl: artifactURL,
			})
			if err != nil {
				return fmt.Errorf("starting OS update: %w", err)
			}

			prog := tui.NewProgress("Updating WendyOS...")
			p := tea.NewProgram(prog)

			go func() {
				for {
					resp, err := stream.Recv()
					if err == io.EOF {
						p.Send(tui.ProgressDoneMsg{})
						return
					}
					if err != nil {
						p.Send(tui.ProgressDoneMsg{Err: err})
						return
					}

					if progress := resp.GetProgress(); progress != nil {
						pct := float64(progress.GetPercent()) / 100.0
						p.Send(tui.ProgressUpdateMsg{Percent: pct})
					}

					if completed := resp.GetCompleted(); completed != nil {
						p.Send(tui.ProgressDoneMsg{})
						return
					}

					if failed := resp.GetFailed(); failed != nil {
						p.Send(tui.ProgressDoneMsg{Err: fmt.Errorf("update failed: %s", failed.GetErrorMessage())})
						return
					}
				}
			}()

			finalModel, err := p.Run()
			if err != nil {
				return fmt.Errorf("TUI error: %w", err)
			}

			model := finalModel.(tui.ProgressModel)
			if model.Err() != nil {
				return model.Err()
			}

			fmt.Println("WendyOS update completed successfully.")
			return nil
		},
	}

	cmd.Flags().StringVar(&artifactURL, "artifact-url", "", "Mender artifact URL")

	return cmd
}
