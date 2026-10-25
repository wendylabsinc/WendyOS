package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/agent/data/live"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/proto"
)

const (
	// catalogSettle is how long the catalog must be quiet before the listing
	// is complete. The agent sends every entry at once after its headers, so
	// a pause this long means it has sent them all.
	catalogSettle = 500 * time.Millisecond
	// catalogMaxWait ends the listing even if entries keep changing.
	catalogMaxWait = 5 * time.Second
)

func newDataStreamsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "streams",
		Short: "List the device's live data streams",
		Long: `List the live data streams on the device, with each stream's schema, state,
rate and the time of its newest record. An app's recording streams
(apps/<app-id>/...) are listed while the app runs, and the agent's own streams
(agent/...) while their producer runs: for example a campaign's markers stream
for each camera its inference scores. A stream is active when it had a record
in the last 10 seconds, and idle otherwise.

The list comes from the catalog the agent publishes on the live stream, under
_streams/. With --json each stream is one line of protobuf JSON.`,
		Example: `  wendy data streams
  wendy data streams --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
				return listLiveStreams(cmd.Context(), client, cmd.OutOrStdout(), jsonOutput, catalogSettle)
			})
		},
	}
}

// listLiveStreams subscribes to the catalog with send_latest, collects the
// entries the agent sends at once, and prints them.
func listLiveStreams(ctx context.Context, client agentpbv2.DataServiceClient, out io.Writer, asJSON bool, settle time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.StreamData(ctx, &agentpbv2.StreamDataRequest{StreamPrefixes: []string{live.CatalogPrefix}, SendLatest: true})
	if err != nil {
		return liveDataError(ctx, err)
	}
	_, _ = stream.Header()
	type received struct {
		record *agentpbv2.DataRecord
		err    error
	}
	records := make(chan received)
	go func() {
		for {
			r, err := stream.Recv()
			select {
			case records <- received{r, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	entries := map[string]*agentpbv2.DataStreamInfo{}
	quiet := time.NewTimer(settle)
	defer quiet.Stop()
	deadline := time.NewTimer(catalogMaxWait)
	defer deadline.Stop()
collect:
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-quiet.C:
			break collect
		case <-deadline.C:
			break collect
		case got := <-records:
			if errors.Is(got.err, io.EOF) {
				break collect
			}
			if got.err != nil {
				return liveDataError(ctx, got.err)
			}
			if !strings.HasPrefix(got.record.GetStreamId(), live.CatalogPrefix) {
				// An agent that predates the catalog ignores the prefix and
				// sends every stream.
				return errors.New("this agent does not publish a stream catalog; update the agent on the target device")
			}
			info := new(agentpbv2.DataStreamInfo)
			if got.record.GetSchema() != live.CatalogSchema || proto.Unmarshal(got.record.GetPayload(), info) != nil {
				continue
			}
			if info.GetState() == agentpbv2.DataStreamInfo_STATE_GONE {
				delete(entries, info.GetStreamId())
			} else {
				entries[info.GetStreamId()] = info
			}
			quiet.Reset(settle)
		}
	}
	sorted := make([]*agentpbv2.DataStreamInfo, 0, len(entries))
	for _, info := range entries {
		sorted = append(sorted, info)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetStreamId() < sorted[j].GetStreamId() })
	if asJSON {
		for _, info := range sorted {
			if err := encodeProtoJSON(out, info); err != nil {
				return err
			}
		}
		return nil
	}
	return writeLiveStreams(out, sorted)
}

func writeLiveStreams(out io.Writer, streams []*agentpbv2.DataStreamInfo) error {
	if len(streams) == 0 {
		_, err := fmt.Fprintln(out, "No live data streams on this device.")
		return err
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STREAM\tSTATE\tRATE\tLAST RECORD\tSCHEMA")
	for _, s := range streams {
		state := strings.ToLower(strings.TrimPrefix(s.GetState().String(), "STATE_"))
		// An app's wendy.json names the schema: keep its controls inert.
		rate, last, schema := "-", "-", printable(s.GetSchema())
		if s.GetRateHz() > 0 {
			rate = fmt.Sprintf("%.1f/s", s.GetRateHz())
		}
		if ts := s.GetLastTimestampUnixNanos(); ts != 0 {
			last = time.Unix(0, ts).Format("15:04:05.000")
		}
		if schema == "" {
			schema = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.GetStreamId(), state, rate, last, schema)
	}
	return w.Flush()
}
