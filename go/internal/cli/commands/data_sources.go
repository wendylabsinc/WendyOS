package commands

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func newDataSourcesCmd() *cobra.Command {
	var kinds []string
	var all bool
	c := &cobra.Command{
		Use:   "sources",
		Short: "List recordable sources",
		Long: "List the cameras, microphones, ROS 2 graphs and built-in sources the device can record, grouped by kind.\n" +
			"Sources marked * are the ones `wendy data record` records when no --source is given.\n" +
			"--json prints every source and every field, including each source's clock domain.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
				r, e := client.Sources(cmd.Context(), &agentpbv2.DataSourcesRequest{})
				if e != nil {
					return e
				}
				wanted := normalizeSourceKinds(kinds)
				if jsonOutput {
					return encodeDataSourcesJSON(cmd.OutOrStdout(), r, wanted)
				}
				return writeDataSources(cmd.OutOrStdout(), r.GetSources(), sourceListing{kinds: wanted, all: all})
			})
		},
	}
	c.Flags().StringSliceVar(&kinds, "kind", nil, "Only list sources of this kind, for example camera or audio (repeatable or comma-separated)")
	c.Flags().BoolVar(&all, "all", false, "List every source, including ROS 2 housekeeping topics and sources past the per-kind limit")
	return c
}

// sourceListing is what the person asked `wendy data sources` to show.
type sourceListing struct {
	// kinds holds the lower-cased kinds asked for with --kind; empty is all.
	kinds []string
	// all lifts both the housekeeping-topic filter and the per-kind limit.
	all bool
}

// maxSourceNameWidth bounds the NAME column. An agent that predates source
// names is shown by its raw detail, an ALSA or PipeWire string that would
// otherwise push the table past a normal terminal width.
const maxSourceNameWidth = 40

// maxSourceStatusWidth bounds the STATUS column, which carries the agent's
// reason for an unusable source.
const maxSourceStatusWidth = 28

// sourceKindFloodLimit is how many sources of one kind are listed before the
// rest are summarised. A Jetson reports 21 audio sources, 20 of which are
// internal audio-DMA routing channels, and they bury everything else.
const sourceKindFloodLimit = 6

// ros2HousekeepingTopics are the topics every ROS 2 node publishes for the
// middleware's own bookkeeping, rather than data anyone set out to record.
// The table hides them unless --all is given; they are still recorded with
// their domain, and --json still lists them.
var ros2HousekeepingTopics = []string{"/parameter_events", "/rosout"}

// sourceGroup is one section of the table. The groups are listed in this
// order, and a source whose kind no group names falls into the last one.
type sourceGroup struct {
	title string
	kind  string
}

var sourceGroups = []sourceGroup{
	{title: "CAMERAS", kind: "camera"},
	{title: "MICROPHONES", kind: "audio"},
	{title: "ROS 2", kind: "ros2"},
	{title: "OTHER"},
}

func sourceGroupIndex(kind string) int {
	for i, group := range sourceGroups {
		if group.kind != "" && strings.EqualFold(group.kind, kind) {
			return i
		}
	}
	return len(sourceGroups) - 1
}

// sourceKindNoun names a kind in the notes under the table.
func sourceKindNoun(kind string, n int) string {
	switch strings.ToLower(kind) {
	case "camera":
		return pluralize(int32(n), "camera", "cameras")
	case "audio":
		return pluralize(int32(n), "microphone", "microphones")
	case "ros2":
		return pluralize(int32(n), "ROS 2 source", "ROS 2 sources")
	}
	return kind + " " + pluralize(int32(n), "source", "sources")
}

// recordedByDefault reports whether `wendy data record` without --source
// records s. It asks the agent's own rule, data.RecordedByDefault, the one
// data.selectSources applies when an episode names no sources, so the marker
// and the recorder cannot drift apart within one release. A device running a
// different agent release applies that release's rule.
func recordedByDefault(s *agentpbv2.DataSource) bool {
	return data.RecordedByDefault(data.Source{ID: s.GetId(), Kind: s.GetKind(), Healthy: s.GetHealthy()})
}

// sourceStatus words a source's state as what the agent actually checked,
// because "healthy" claimed more than any check delivers. The checks, kind by
// kind, are the ones the agent's data adapters apply
// (go/internal/agent/services/data_*_adapter.go and data.DiscoverSources):
//
//	connected   a local camera (V4L2, CSI or ROS 2 loopback) answered a capture
//	            query when listed; nothing tests that it delivers a picture
//	online      a network camera answered the last discovery probe and has a
//	            stored login; the login itself is not tried
//	detected    PipeWire or ALSA lists the microphone as a capture device; nothing
//	            tests that it hears anything
//	reachable   `ros2 topic list` answered on the ROS 2 domain
//	listed      `ros2 topic list` lists the topic
//	built in    application records and telemetry, which need no hardware
//
// An unusable source shows the agent's reason instead, such as "no login
// stored". An agent that predates reasons, or a kind this CLI does not know,
// keeps the words it always had: healthy and unhealthy.
func sourceStatus(s *agentpbv2.DataSource) string {
	if !s.GetHealthy() {
		if reason := s.GetUnusableReason(); reason != "" {
			return reason
		}
		return "unhealthy"
	}
	if s.GetName() == "" {
		// An agent this old may have applied different checks; do not word its
		// verdict as if it had applied these.
		return "healthy"
	}
	id, kind := s.GetId(), strings.ToLower(s.GetKind())
	switch {
	case kind == "camera" && strings.HasPrefix(id, "v4l2:"):
		return "connected"
	case kind == "camera" && strings.HasPrefix(id, "ipcamera:"):
		return "online"
	case kind == "audio":
		return "detected"
	case kind == "ros2":
		if _, topic, ok := data.ParseROS2SourceID(id); ok && topic != "" {
			return "listed"
		}
		return "reachable"
	case kind == "application" || kind == "telemetry":
		return "built in"
	}
	return "healthy"
}

// sourceName is the NAME column: the agent's friendly name, or, from an agent
// that predates names, its raw detail and failing that the ID.
func sourceName(s *agentpbv2.DataSource) string {
	if name := strings.TrimSpace(s.GetName()); name != "" {
		return name
	}
	if detail := strings.TrimSpace(s.GetDetail()); detail != "" {
		return detail
	}
	return s.GetId()
}

// isHousekeepingTopic reports whether s is one of ros2HousekeepingTopics.
func isHousekeepingTopic(s *agentpbv2.DataSource) (string, bool) {
	if !strings.EqualFold(s.GetKind(), "ros2") {
		return "", false
	}
	_, topic, ok := data.ParseROS2SourceID(s.GetId())
	if !ok || topic == "" {
		return "", false
	}
	for _, hidden := range ros2HousekeepingTopics {
		if topic == hidden {
			return topic, true
		}
	}
	return "", false
}

// encodeDataSourcesJSON keeps --json a machine contract: without --kind it is
// byte-for-byte the response the device sent, never filtered and never
// summarised, because scripts already consume it. With --kind it carries the
// filtered set, which is what the caller asked for. Fields only ever get added
// to it, never renamed or removed.
func encodeDataSourcesJSON(out io.Writer, response *agentpbv2.DataSourcesResponse, wanted []string) error {
	if len(wanted) == 0 {
		return encodeProtoJSON(out, response)
	}
	return encodeProtoJSON(out, &agentpbv2.DataSourcesResponse{Sources: filterSourcesByKind(response.GetSources(), wanted)})
}

func normalizeSourceKinds(kinds []string) []string {
	var out []string
	for _, kind := range kinds {
		if k := strings.ToLower(strings.TrimSpace(kind)); k != "" {
			out = append(out, k)
		}
	}
	return out
}

func filterSourcesByKind(sources []*agentpbv2.DataSource, wanted []string) []*agentpbv2.DataSource {
	if len(wanted) == 0 {
		return sources
	}
	var out []*agentpbv2.DataSource
	for _, s := range sources {
		for _, kind := range wanted {
			if strings.EqualFold(s.GetKind(), kind) {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// truncateRunes cuts s to width runes, the last three of them dots.
func truncateRunes(s string, width int) string {
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width-3]) + "..."
}

func padRunes(s string, width int) string {
	if n := len([]rune(s)); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

type sourceRow struct {
	recorded bool
	name     string
	status   string
	id       string
}

// writeDataSources renders the source table: one section per kind, each row a
// marker, a name, a status and the ID. The ID is the last column on purpose.
// It is the longest field, a ROS 2 topic or a microphone's PipeWire node name,
// and as the last column it never pads the columns after it, so a long one
// costs a long line rather than a wide table.
func writeDataSources(out io.Writer, sources []*agentpbv2.DataSource, listing sourceListing) error {
	shown := filterSourcesByKind(sources, listing.kinds)
	if len(shown) == 0 {
		if len(sources) == 0 {
			_, err := fmt.Fprintln(out, "No recordable sources reported by the device.")
			return err
		}
		_, err := fmt.Fprintf(out, "No sources of kind %s. Kinds present on this device: %s.\n",
			strings.Join(quoteAll(listing.kinds), ", "), strings.Join(presentSourceKinds(sources), ", "))
		return err
	}

	requested := make(map[string]bool, len(listing.kinds))
	for _, kind := range listing.kinds {
		requested[kind] = true
	}

	// Housekeeping topics go first, so they neither fill a kind's limit nor
	// count among the sources it summarises.
	var hiddenTopics []string
	hiddenSeen := map[string]bool{}
	hiddenCount := 0
	var visible []*agentpbv2.DataSource
	for _, s := range shown {
		if topic, ok := isHousekeepingTopic(s); ok && !listing.all {
			hiddenCount++
			if !hiddenSeen[topic] {
				hiddenSeen[topic] = true
				hiddenTopics = append(hiddenTopics, topic)
			}
			continue
		}
		visible = append(visible, s)
	}

	total := map[string]int{}
	for _, s := range visible {
		total[strings.ToLower(s.GetKind())]++
	}
	groups := make([][]sourceRow, len(sourceGroups))
	omitted := map[string]int{}
	var floodedOrder []string
	seen := map[string]int{}
	legacyAgent := false
	anyRecorded := false
	for _, s := range visible {
		kind := strings.ToLower(s.GetKind())
		if !listing.all && !requested[kind] && total[kind] > sourceKindFloodLimit && seen[kind] >= sourceKindFloodLimit {
			if omitted[kind] == 0 {
				floodedOrder = append(floodedOrder, kind)
			}
			omitted[kind]++
			continue
		}
		seen[kind]++
		if s.GetName() == "" {
			legacyAgent = true
		}
		row := sourceRow{
			recorded: recordedByDefault(s),
			name:     truncateRunes(sourceName(s), maxSourceNameWidth),
			status:   truncateRunes(sourceStatus(s), maxSourceStatusWidth),
			id:       s.GetId(),
		}
		anyRecorded = anyRecorded || row.recorded
		g := sourceGroupIndex(kind)
		groups[g] = append(groups[g], row)
	}

	nameWidth, statusWidth := 0, len([]rune("STATUS"))
	for i, rows := range groups {
		if len(rows) == 0 {
			continue
		}
		nameWidth = max(nameWidth, len([]rune(sourceGroups[i].title)))
		for _, row := range rows {
			nameWidth = max(nameWidth, len([]rune(row.name)))
			statusWidth = max(statusWidth, len([]rune(row.status)))
		}
	}

	var b strings.Builder
	first := true
	for i, rows := range groups {
		if len(rows) == 0 {
			continue
		}
		if !first {
			b.WriteString("\n")
		}
		first = false
		fmt.Fprintf(&b, "  %s  %s  ID\n", padRunes(sourceGroups[i].title, nameWidth), padRunes("STATUS", statusWidth))
		for _, row := range rows {
			marker := " "
			if row.recorded {
				marker = "*"
			}
			fmt.Fprintf(&b, "%s %s  %s  %s\n", marker, padRunes(row.name, nameWidth), padRunes(row.status, statusWidth), row.id)
		}
	}

	var notes []string
	if anyRecorded {
		notes = append(notes, "* recorded by `wendy data record` when no --source is given")
	}
	if hiddenCount > 0 {
		notes = append(notes, fmt.Sprintf("%d ROS 2 housekeeping %s hidden (%s); they are still recorded with their domain. --all lists them.",
			hiddenCount, pluralize(int32(hiddenCount), "topic", "topics"), strings.Join(hiddenTopics, ", ")))
	}
	for _, kind := range floodedOrder {
		notes = append(notes, fmt.Sprintf("... %d more %s not listed (--all or --kind %s lists them)",
			omitted[kind], sourceKindNoun(kind, omitted[kind]), kind))
	}
	if legacyAgent {
		notes = append(notes, "This device's agent predates source names: NAME shows its raw detail, and STATUS its own healthy or unhealthy.")
	}
	if len(notes) > 0 {
		b.WriteString("\n")
		for _, note := range notes {
			b.WriteString(note)
			b.WriteString("\n")
		}
	}
	b.WriteString("\nRecord some: wendy data record --source <ID> [--source <ID> ...]\n")
	b.WriteString("More:        wendy data sources --json\n")
	_, err := io.WriteString(out, b.String())
	return err
}

func quoteAll(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, strconv.Quote(v))
	}
	return out
}

// presentSourceKinds lists the distinct kinds in first-seen order so the
// message names what the device actually reported.
func presentSourceKinds(sources []*agentpbv2.DataSource) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range sources {
		kind := s.GetKind()
		if kind == "" || seen[kind] {
			continue
		}
		seen[kind] = true
		out = append(out, kind)
	}
	return out
}

// warnLegacyMicrophoneIDs tells the person on the terminal when they typed an
// old-style microphone ID ("audio:<n>") that the device now resolves through a
// deprecated alias. The agent resolves it either way and logs the same
// warning, but its log is not where anyone is looking when they run a command.
//
// It costs a Sources call, so it is made only when such an ID was typed, and it
// stays quiet against an agent that still lists that very ID (one that predates
// stable microphone IDs) or when the listing cannot be read.
func warnLegacyMicrophoneIDs(ctx context.Context, out io.Writer, client agentpbv2.DataServiceClient, typed []string) {
	var legacy []string
	seen := map[string]bool{}
	for _, id := range typed {
		if data.IsLegacyAudioSourceID(id) && !seen[id] {
			seen[id] = true
			legacy = append(legacy, id)
		}
	}
	if len(legacy) == 0 {
		return
	}
	response, err := client.Sources(ctx, &agentpbv2.DataSourcesRequest{})
	if err != nil {
		return
	}
	listed := map[string]bool{}
	named := false
	for _, s := range response.GetSources() {
		listed[s.GetId()] = true
		named = named || s.GetName() != ""
	}
	if !named {
		return
	}
	for _, id := range legacy {
		if listed[id] {
			continue
		}
		fmt.Fprintf(out, "Warning: %s is an old-style microphone ID, a number that can change after a reboot or a replug. "+
			"If it still names a microphone, the device accepts it for now. Use the stable ID from `wendy data sources --kind audio` instead.\n", id)
	}
}
