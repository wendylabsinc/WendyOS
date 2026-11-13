package commands

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// pickEpisodeFn opens the episode picker. Tests replace it to choose a row
// without a terminal.
var pickEpisodeFn = pickFromItemsWithColumns

// episodePickerAllowed reports whether a missing episode ID may be answered
// with a picker: a terminal on both ends, no --json, no --non-interactive.
func episodePickerAllowed(nonInteractive bool) bool {
	return !nonInteractive && !jsonOutput && isInteractiveTerminal()
}

// requireEpisodeArg refuses a missing episode ID when no picker can be shown.
// It runs before the device is dialed, so a script or an agent gets an error
// naming where IDs come from rather than a prompt nobody will answer. It stays
// a usage error, as the argument check it replaces was, so the exit status
// does not change.
func requireEpisodeArg(args []string, interactive bool, verb string) error {
	if len(args) > 0 || interactive {
		return nil
	}
	return withNextSteps(
		usageErrorf("an episode ID is required in non-interactive or --json mode"),
		fmt.Sprintf("List episodes with 'wendy data episodes', then run 'wendy data %s <episode>'.", verb))
}

// dataDialOptions keeps --non-interactive from opening a device picker either.
func dataDialOptions(nonInteractive bool) []resolveOption {
	if nonInteractive {
		return []resolveOption{NonInteractive()}
	}
	return nil
}

// resolveEpisodeArg returns the episode named on the command line, or lets the
// user choose one of the device's episodes. ok is false when the device holds
// none: a plain message is printed in place of an empty picker, and the
// command has nothing left to do. The device lists only sealed episodes, so the
// message also covers a recording that is still running.
func resolveEpisodeArg(ctx context.Context, client agentpbv2.DataServiceClient, args []string, out io.Writer, title string) (id string, ok bool, err error) {
	if len(args) > 0 {
		return args[0], true, nil
	}
	response, err := client.Episodes(ctx, &agentpbv2.DataEpisodesRequest{})
	if err != nil {
		return "", false, err
	}
	if len(response.GetEpisodes()) == 0 {
		_, err = fmt.Fprintln(out, "No finalized episodes on this device yet. Record one with 'wendy data record', or seal one in progress with 'wendy data stop'.")
		return "", false, err
	}
	id, err = pickEpisodeFn(title, episodePickerItems(response.GetEpisodes()), episodePickerColumns())
	return id, err == nil, err
}

// episodePickerItems builds the rows newest first. The Linux agent already
// lists episodes in that order, but the CLI does not rely on every agent doing
// so. SortKey pins the order, because the picker otherwise sorts rows by Name.
// The picker also merges rows that share a DedupKey, falling back to Name.
// Both hold the ID, not the episode's name (Description), because recordings
// often share a name.
func episodePickerItems(episodes []*agentpbv2.DataEpisode) []tui.PickerItem {
	sorted := slices.Clone(episodes)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].GetStartedUnixNanos() > sorted[j].GetStartedUnixNanos()
	})
	items := make([]tui.PickerItem, 0, len(sorted))
	for i, e := range sorted {
		items = append(items, tui.PickerItem{
			Name:        e.GetId(),
			Description: e.GetName(),
			Type:        e.GetState(),
			Size:        formatBytes(e.GetSizeBytes()),
			Comments:    episodeStarted(e.GetStartedUnixNanos()),
			DedupKey:    e.GetId(),
			SortKey:     fmt.Sprintf("%06d", i),
			Value:       e.GetId(),
		})
	}
	return items
}

func episodePickerColumns() []tui.PickerColumn {
	return []tui.PickerColumn{
		{Title: "Episode", MinWidth: 12, Required: true, Value: func(i tui.PickerItem) string { return i.Name }},
		{Title: "Name", MinWidth: 8, Value: func(i tui.PickerItem) string { return i.Description }},
		{Title: "State", MinWidth: 8, Value: func(i tui.PickerItem) string { return i.Type }},
		{Title: "Size", MinWidth: 8, Value: func(i tui.PickerItem) string { return i.Size }},
		{Title: "Started", MinWidth: 12, Value: func(i tui.PickerItem) string { return i.Comments }},
	}
}

// episodeStarted renders an episode's start in local time, which is what the
// person choosing it remembers; the ID's prefix already carries UTC.
func episodeStarted(unixNanos int64) string {
	if unixNanos <= 0 {
		return "-"
	}
	return time.Unix(0, unixNanos).Local().Format("2006-01-02 15:04:05")
}
