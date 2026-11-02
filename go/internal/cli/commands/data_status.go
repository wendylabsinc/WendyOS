package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// olderAgentInProgressNote is printed when the agent predates
// DataStatusResponse.in_progress. Such an agent reports at most one open
// episode, so a second concurrent recording (a campaign beside a manual one)
// may be missing, and nothing about the one it does report beyond its name and
// start time.
const olderAgentInProgressNote = "This agent reports one recording at a time, without its trigger, sources or size. Update it with `wendy device update` to see them all."

// noInProgressReportNote is printed when the agent does not serve
// DataService.Status at all.
const noInProgressReportNote = "This agent doesn't report recordings in progress."

// inProgressStateNotes explains the two states an episode passes through after
// capture stops, which a person would otherwise read as "stuck".
var inProgressStateNotes = map[string]string{
	"draining": "capture stopped; accepting late application records",
	"sealing":  "capture stopped; writing playable video and checksums",
}

func newDataStatusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Show the recordings in progress on the device", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
			return runDataStatus(cmd.Context(), client, cmd.OutOrStdout(), jsonOutput, time.Now())
		})
	}}
}

func runDataStatus(ctx context.Context, client agentpbv2.DataServiceClient, out io.Writer, asJSON bool, now time.Time) error {
	response, err := client.Status(ctx, &agentpbv2.DataStatusRequest{})
	if status.Code(err) == codes.Unimplemented {
		return errors.New("this agent doesn't report recordings in progress; update it with `wendy device update`")
	}
	if err != nil {
		return err
	}
	if asJSON {
		return encodeProtoJSON(out, response)
	}
	return writeDataStatus(out, response, now)
}

// runDataEpisodes lists finalized episodes with the ones still in progress
// above them. --json is the Episodes response, with in_progress filled from
// Status when the agent's Episodes response predates that field; the note that
// says so goes to errOut there, so stdout stays one JSON document.
func runDataEpisodes(ctx context.Context, client agentpbv2.DataServiceClient, out, errOut io.Writer, asJSON bool, now time.Time) error {
	response, err := client.Episodes(ctx, &agentpbv2.DataEpisodesRequest{})
	if err != nil {
		return err
	}
	note := addInProgressFromStatus(ctx, client, response)
	if asJSON {
		if note != "" {
			fmt.Fprintln(errOut, note)
		}
		return encodeProtoJSON(out, response)
	}
	return writeDataEpisodes(out, response, now, note)
}

// inProgressFromStatus returns the episodes a Status response reports as in
// progress, and whether that list is complete. An agent that predates
// in_progress reports at most one open episode, in active. Its list is complete
// when it is empty, because that agent sets active whenever anything is
// recording or draining.
func inProgressFromStatus(response *agentpbv2.DataStatusResponse) (episodes []*agentpbv2.DataEpisode, complete bool) {
	if response.GetReportsInProgress() {
		return response.GetInProgress(), true
	}
	if response.GetActive() == nil {
		return nil, true
	}
	return []*agentpbv2.DataEpisode{response.GetActive()}, false
}

// addInProgressFromStatus covers agents whose Episodes response predates
// in_progress. When the response neither says it reports in progress nor lists
// anything there, it asks Status, which every agent with a DataService has
// served, though an older one reports only one episode there. It returns a note
// for the reader when the result may be incomplete, and "" otherwise.
func addInProgressFromStatus(ctx context.Context, client agentpbv2.DataServiceClient, response *agentpbv2.DataEpisodesResponse) string {
	if response.GetReportsInProgress() || len(response.GetInProgress()) > 0 {
		return ""
	}
	statusResponse, err := client.Status(ctx, &agentpbv2.DataStatusRequest{})
	if status.Code(err) == codes.Unimplemented {
		return noInProgressReportNote
	}
	if err != nil {
		return fmt.Sprintf("Could not check for recordings in progress: %v", err)
	}
	episodes, complete := inProgressFromStatus(statusResponse)
	// The two calls are not atomic: an episode that finished between them is
	// already in the finalized list.
	finalized := make(map[string]bool, len(response.GetEpisodes()))
	for _, episode := range response.GetEpisodes() {
		finalized[episode.GetId()] = true
	}
	for _, episode := range episodes {
		if !finalized[episode.GetId()] {
			response.InProgress = append(response.InProgress, episode)
		}
	}
	if !complete && len(response.InProgress) > 0 {
		return olderAgentInProgressNote
	}
	return ""
}

// inProgressDetailed reports whether an in-progress episode came from an agent
// that fills the in-progress fields. Such an agent always sets trigger_reason,
// because an episode started without one is recorded as "manual".
func inProgressDetailed(episode *agentpbv2.DataEpisode) bool {
	return episode.GetTriggerReason() != ""
}

// inProgressDuration is how long an episode has recorded. An older agent does
// not report it, so it is estimated from the device's start time and this
// machine's clock, and marked with "~" because the two clocks may disagree.
func inProgressDuration(episode *agentpbv2.DataEpisode, now time.Time) string {
	if inProgressDetailed(episode) {
		return time.Duration(episode.GetDurationNanos()).Round(time.Second).String()
	}
	if started := episode.GetStartedUnixNanos(); started > 0 && now.UnixNano() > started {
		return "~" + time.Duration(now.UnixNano()-started).Round(time.Second).String()
	}
	return "-"
}

func inProgressSize(episode *agentpbv2.DataEpisode) string {
	if !inProgressDetailed(episode) {
		return "-"
	}
	return fmt.Sprint(episode.GetSizeBytes())
}

// inProgressTrigger is the short form for the episodes table: the campaign, or
// "manual" for `wendy data record`.
func inProgressTrigger(episode *agentpbv2.DataEpisode) string {
	switch {
	case episode.GetCampaignName() != "":
		return "campaign " + episode.GetCampaignName()
	case episode.GetTriggerReason() != "":
		return episode.GetTriggerReason()
	default:
		return "-"
	}
}

// writeDataEpisodes renders the episodes table. Episodes in progress come first
// and add DURATION and TRIGGER columns, which only they fill; with none in
// progress the table has its original four columns.
func writeDataEpisodes(out io.Writer, response *agentpbv2.DataEpisodesResponse, now time.Time, note string) error {
	open := response.GetInProgress()
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	header := "EPISODE\tSTATE\tSIZE\tNAME"
	if len(open) > 0 {
		header += "\tDURATION\tTRIGGER"
	}
	fmt.Fprintln(w, header)
	for _, episode := range open {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", episode.GetId(), episode.GetState(), inProgressSize(episode), episode.GetName(), inProgressDuration(episode, now), inProgressTrigger(episode))
	}
	for _, episode := range response.GetEpisodes() {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", episode.GetId(), episode.GetState(), episode.GetSizeBytes(), episode.GetName())
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if note != "" {
		_, err := fmt.Fprintf(out, "\n%s\n", note)
		return err
	}
	return nil
}

// writeDataStatus prints each episode in progress in the same "Field: value"
// form as `wendy data inspect`.
func writeDataStatus(out io.Writer, response *agentpbv2.DataStatusResponse, now time.Time) error {
	episodes, complete := inProgressFromStatus(response)
	if len(episodes) == 0 {
		_, err := fmt.Fprintln(out, "No recording in progress.")
		return err
	}
	for i, episode := range episodes {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "Episode: %s", episode.GetId())
		if episode.GetName() != "" {
			fmt.Fprintf(out, " (%s)", episode.GetName())
		}
		fmt.Fprintf(out, "\nState: %s", episode.GetState())
		if note := inProgressStateNotes[episode.GetState()]; note != "" {
			fmt.Fprintf(out, " (%s)", note)
		}
		fmt.Fprintln(out)
		if started := episode.GetStartedUnixNanos(); started > 0 {
			fmt.Fprintf(out, "Started: %s (device clock)\n", time.Unix(0, started).UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(out, "Duration: %s\n", inProgressDuration(episode, now))
		if !inProgressDetailed(episode) {
			continue
		}
		fmt.Fprintf(out, "Trigger: %s", episode.GetTriggerReason())
		if episode.GetCampaignName() != "" {
			fmt.Fprintf(out, " (campaign %s)", episode.GetCampaignName())
		}
		sources := strings.Join(episode.GetSources(), ", ")
		if sources == "" {
			sources = "none"
		}
		fmt.Fprintf(out, "\nSources: %s\nSize so far: %d bytes\n", sources, episode.GetSizeBytes())
	}
	if !complete {
		_, err := fmt.Fprintf(out, "\n%s\n", olderAgentInProgressNote)
		return err
	}
	return nil
}

// printRecordNextSteps follows `wendy data record`: the episode keeps
// recording on the device after the command returns, and nothing else on
// screen says how to end it or check on it. device is the --device the episode
// was started on. It is repeated in both commands, because without it a copied
// `wendy data stop` goes to the saved default device and stops whatever that
// one is recording.
func printRecordNextSteps(out io.Writer, device string) {
	target := ""
	if device != "" {
		target = " --device " + shellQuoteArg(device)
	}
	fmt.Fprintf(out, "%s %s%s %s\n", tui.Dim("Stop it with"), tui.Command("wendy data stop"+target), tui.Dim("; check on it with"), tui.Command("wendy data status"+target))
}
