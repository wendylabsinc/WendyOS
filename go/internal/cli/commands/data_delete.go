package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// confirmDeleteEpisodesFn asks before anything is deleted, defaulting to no.
// Tests replace it to answer without a terminal.
var confirmDeleteEpisodesFn = func(question string) (bool, error) {
	return tui.ConfirmDefaultNo(question, tea.WithOutput(os.Stderr))
}

func newDataDeleteCmd() *cobra.Command {
	var all, force, nonInteractive bool
	c := &cobra.Command{Use: "delete [episode...]", Aliases: []string{"rm"}, Short: "Permanently delete episodes from the device",
		Long: "Delete finalized episodes from the device: each one's manifest and every file it recorded.\n" +
			"This cannot be undone, and an episode that has not been uploaded is lost for good.\n" +
			"Name the episodes to delete, or pass --all to delete every finalized episode. Without either, select one of the\n" +
			"device's episodes. A recording still in progress is never deleted: stop it with 'wendy data stop' first.\n" +
			"Deleting asks for confirmation; --force skips it, and is required in non-interactive or --json mode.",
		Args: cobra.ArbitraryArgs, RunE: func(cmd *cobra.Command, args []string) error {
			if all && len(args) > 0 {
				return usageErrorf("name episodes to delete or pass --all, not both")
			}
			interactive := episodePickerAllowed(nonInteractive)
			if !all {
				if e := requireEpisodeArg(args, interactive, "delete"); e != nil {
					return e
				}
			}
			if !force && !interactive {
				return withNextSteps(
					usageErrorf("refusing to delete episodes without confirmation: no interactive terminal to confirm on"),
					"Pass --force to delete without confirmation: wendy data delete --force <episode>...")
			}
			return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
				return runDataDelete(cmd.Context(), client, cmd.OutOrStdout(), args, all, force)
			}, dataDialOptions(nonInteractive)...)
		}}
	c.Flags().BoolVar(&all, "all", false, "Delete every finalized episode on the device")
	c.Flags().BoolVar(&force, "force", false, "Delete without asking for confirmation (required without an interactive terminal)")
	c.Flags().BoolVar(&nonInteractive, "non-interactive", false, "Disable terminal prompts; requires episode IDs or --all, and --force")
	return c
}

func runDataDelete(ctx context.Context, client agentpbv2.DataServiceClient, out io.Writer, args []string, all, force bool) error {
	listed, err := client.Episodes(ctx, &agentpbv2.DataEpisodesRequest{})
	if err != nil {
		return err
	}
	targets, err := deleteTargets(ctx, client, out, listed, args, all)
	if err != nil || len(targets) == 0 {
		return err
	}
	if !force {
		writeDeleteTargets(out, targets)
		question := fmt.Sprintf("Delete episode %s? This cannot be undone, and an episode that has not been uploaded is lost for good.", targets[0].GetId())
		if len(targets) > 1 {
			question = fmt.Sprintf("Delete these %d episodes? This cannot be undone, and an episode that has not been uploaded is lost for good.", len(targets))
		}
		confirmed, err := confirmDeleteEpisodesFn(question)
		if err != nil {
			if errors.Is(err, tui.ErrCancelled) {
				return ErrUserCancelled
			}
			return err
		}
		if !confirmed {
			_, err = fmt.Fprintln(out, "Cancelled; nothing was deleted.")
			return err
		}
	}

	var deleted int
	var freed int64
	var failures []string
	for _, target := range targets {
		response, err := client.DeleteEpisode(ctx, &agentpbv2.DataDeleteEpisodeRequest{Episode: target.GetId()})
		if status.Code(err) == codes.Unimplemented {
			return errors.New("this device's agent does not support `wendy data delete`; update it with `wendy device update`")
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", target.GetId(), status.Convert(err).Message()))
			continue
		}
		deleted++
		freed += response.GetEpisode().GetSizeBytes()
		if jsonOutput {
			if err := encodeProtoJSON(out, response); err != nil {
				return err
			}
			continue
		}
		episode := response.GetEpisode()
		fmt.Fprintf(out, "Deleted episode %s (%s, %s).\n", episode.GetId(), episode.GetName(), formatBytes(episode.GetSizeBytes()))
	}
	if !jsonOutput && deleted > 1 {
		fmt.Fprintf(out, "Deleted %d episodes, %s in all.\n", deleted, formatBytes(freed))
	}
	if all && !jsonOutput && len(listed.GetInProgress()) > 0 {
		fmt.Fprintf(out, "%d recording(s) still in progress were not deleted; stop one with 'wendy data stop', then delete it.\n", len(listed.GetInProgress()))
	}
	if len(failures) > 0 {
		return fmt.Errorf("could not delete %d of %d episode(s):\n  %s", len(failures), len(targets), strings.Join(failures, "\n  "))
	}
	return nil
}

// deleteTargets resolves what the command line asked to delete against the
// device's own list, before anything is deleted: a mistyped or unfinished
// episode stops the command, so a typo among several IDs deletes none of them.
// An empty result with a nil error means there is nothing to delete, and the
// reason has been printed.
func deleteTargets(ctx context.Context, client agentpbv2.DataServiceClient, out io.Writer, listed *agentpbv2.DataEpisodesResponse, args []string, all bool) ([]*agentpbv2.DataEpisode, error) {
	finalized := listed.GetEpisodes()
	if all {
		if len(finalized) == 0 {
			_, err := fmt.Fprintln(out, "No finalized episodes on this device; nothing to delete.")
			return nil, err
		}
		return finalized, nil
	}
	if len(args) == 0 {
		id, ok, err := resolveEpisodeArg(ctx, client, args, out, "Select an episode to delete")
		if err != nil || !ok {
			return nil, err
		}
		args = []string{id}
	}
	var targets []*agentpbv2.DataEpisode
	var seen []string
	for _, id := range args {
		if slices.Contains(seen, id) {
			continue
		}
		seen = append(seen, id)
		if i := slices.IndexFunc(finalized, func(e *agentpbv2.DataEpisode) bool { return e.GetId() == id }); i >= 0 {
			targets = append(targets, finalized[i])
			continue
		}
		if i := slices.IndexFunc(listed.GetInProgress(), func(e *agentpbv2.DataEpisode) bool { return e.GetId() == id }); i >= 0 {
			return nil, fmt.Errorf("episode %s is still %s; stop it with 'wendy data stop' or wait for it to be sealed, then delete it; nothing was deleted",
				id, listed.GetInProgress()[i].GetState())
		}
		return nil, withNextSteps(fmt.Errorf("no finalized episode %s on this device; nothing was deleted", id),
			"List the device's episodes with 'wendy data episodes'.")
	}
	return targets, nil
}

func writeDeleteTargets(out io.Writer, targets []*agentpbv2.DataEpisode) {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "EPISODE\tNAME\tSIZE\tSTARTED")
	for _, episode := range targets {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", episode.GetId(), episode.GetName(), formatBytes(episode.GetSizeBytes()), episodeStarted(episode.GetStartedUnixNanos()))
	}
	_ = w.Flush()
}
