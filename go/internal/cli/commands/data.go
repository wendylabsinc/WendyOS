package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func newDataCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "data", Short: "Record and retrieve synchronized device data"}
	cmd.AddCommand(newDataSourcesCmd(), newDataRecordCmd(), newDataStopCmd(), newDataEpisodesCmd(), newDataInspectCmd(), newDataDownloadCmd(), newDataCampaignCmd(), newDataEnrollCmd())
	return cmd
}

func newDataCampaignCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "campaign", Short: "Deploy and run flight-recorder campaign plans"}
	cmd.AddCommand(newDataCampaignDeployCmd(), newDataCampaignListCmd(), newDataCampaignInspectCmd(), newDataCampaignTriggerCmd())
	return cmd
}

func newDataCampaignDeployCmd() *cobra.Command {
	var skipCloudRegistration bool
	command := &cobra.Command{Use: "deploy [campaign.yaml]", Short: "Validate, persist, and arm a campaign on the connected device", Long: "Deploy a campaign YAML file. Without a path, select a .yaml or .yml file from the current directory.\nNon-interactive and --json invocations require an explicit path.", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		path, err := resolveCampaignDeployFile(args, !jsonOutput && isInteractiveTerminal(), pickFromItems)
		if err != nil {
			return err
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading campaign %q: %w", path, err)
		}
		plan, err := data.ParseCampaign(contents)
		if err != nil {
			return err
		}
		conn, err := connectToAgent(cmd.Context())
		if err != nil {
			return err
		}
		defer conn.Close()
		if err := registerCloudApps(cmd.Context(), conn, []string{"campaign:" + plan.Name}, skipCloudRegistration); err != nil {
			return err
		}
		campaign, err := conn.DataService.CampaignDeploy(cmd.Context(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: contents})
		if err != nil {
			return err
		}
		if jsonOutput {
			_, err = cmd.OutOrStdout().Write(campaign.GetPlanJson())
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Campaign %s: %s (revision %s)\n", campaign.GetName(), campaign.GetState(), shortRevision(campaign.GetRevision()))
		for _, warning := range campaign.GetWarnings() {
			fmt.Fprintf(cmd.OutOrStdout(), "Warning: %s\n", warning)
		}
		return nil
	}}
	command.Flags().BoolVar(&skipCloudRegistration, "skip-cloud-registration", false, "Deploy without registering the campaign in Cloud (offline use)")
	return command
}

func resolveCampaignDeployFile(args []string, interactive bool, pick func(string, []tui.PickerItem) (string, error)) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	const usage = "pass a file path, for example: wendy data campaign deploy campaign.yaml"
	entries, err := os.ReadDir(".")
	if err != nil {
		return "", fmt.Errorf("listing campaign files in the current directory: %w; %s", err, usage)
	}
	var items []tui.PickerItem
	var names []string
	for _, entry := range entries {
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		info, err := os.Stat(entry.Name())
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		names = append(names, entry.Name())
		items = append(items, tui.PickerItem{Name: entry.Name(), Value: entry.Name()})
	}
	if len(items) == 0 {
		return "", fmt.Errorf("no .yaml or .yml files found in the current directory; %s", usage)
	}
	if !interactive {
		return "", fmt.Errorf("a campaign YAML path is required in non-interactive or --json mode; %s\nYAML files in the current directory: %s", usage, strings.Join(names, ", "))
	}
	return pick("Select a campaign YAML file", items)
}

func newDataCampaignListCmd() *cobra.Command {
	return &cobra.Command{Use: "list", Aliases: []string{"ls"}, Short: "List deployed campaigns", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
			response, err := client.Campaigns(cmd.Context(), &agentpbv2.DataCampaignsRequest{})
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(response)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "CAMPAIGN\tSTATE\tREVISION\tFLEET")
			for _, campaign := range response.GetCampaigns() {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", campaign.GetName(), campaign.GetState(), shortRevision(campaign.GetRevision()), campaign.GetFleet())
			}
			return nil
		})
	}}
}

func newDataCampaignInspectCmd() *cobra.Command {
	return &cobra.Command{Use: "inspect <campaign>", Short: "Show the canonical deployed campaign plan", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
			campaign, err := client.CampaignInspect(cmd.Context(), &agentpbv2.DataCampaignInspectRequest{Name: args[0]})
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(campaign.GetPlanJson())
			return err
		})
	}}
}

func newDataCampaignTriggerCmd() *cobra.Command {
	var reason string
	command := &cobra.Command{Use: "trigger <campaign>", Short: "Trigger a deployed campaign now", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
			episode, err := client.CampaignTrigger(cmd.Context(), &agentpbv2.DataCampaignTriggerRequest{Name: args[0], Reason: reason})
			if err != nil {
				return err
			}
			return printEpisode(cmd, episode)
		})
	}}
	command.Flags().StringVar(&reason, "reason", "manual", "Trigger reason stored in the Episode manifest")
	return command
}

func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

func withDataClient(ctx context.Context, fn func(agentpbv2.DataServiceClient) error) error {
	conn, err := connectToAgent(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return fn(conn.DataService)
}

func newDataSourcesCmd() *cobra.Command {
	var kinds []string
	c := &cobra.Command{Use: "sources", Short: "List recordable sources", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
			r, e := client.Sources(cmd.Context(), &agentpbv2.DataSourcesRequest{})
			if e != nil {
				return e
			}
			wanted := normalizeSourceKinds(kinds)
			if jsonOutput {
				return encodeDataSourcesJSON(cmd.OutOrStdout(), r, wanted)
			}
			return writeDataSources(cmd.OutOrStdout(), r.GetSources(), wanted)
		})
	}}
	c.Flags().StringSliceVar(&kinds, "kind", nil, "Only list sources of this kind, for example camera or audio (repeatable or comma-separated)")
	return c
}

// maxSourceDetailWidth bounds the DETAIL column so an ALSA card description
// cannot push the table past a normal terminal width.
const maxSourceDetailWidth = 48

// sourceKindFloodLimit is how many sources of one kind are listed before the
// rest are summarised. A Jetson reports 21 audio sources, 20 of which are
// internal audio-DMA routing channels, and they bury everything else.
const sourceKindFloodLimit = 6

// encodeDataSourcesJSON keeps --json a machine contract: without --kind it is
// byte-for-byte the response the device sent, never filtered and never
// summarised, because scripts already consume it. With --kind it carries the
// filtered set, which is what the caller asked for.
func encodeDataSourcesJSON(out io.Writer, response *agentpbv2.DataSourcesResponse, wanted []string) error {
	if len(wanted) == 0 {
		return json.NewEncoder(out).Encode(response)
	}
	return json.NewEncoder(out).Encode(&agentpbv2.DataSourcesResponse{Sources: filterSourcesByKind(response.GetSources(), wanted)})
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

func truncateSourceDetail(detail string) string {
	runes := []rune(detail)
	if len(runes) <= maxSourceDetailWidth {
		return detail
	}
	return string(runes[:maxSourceDetailWidth-3]) + "..."
}

// writeDataSources renders the source table. wanted holds the lower-cased kinds
// the user asked for; kinds named there are never summarised, because asking for
// a kind is asking to see all of it.
func writeDataSources(out io.Writer, sources []*agentpbv2.DataSource, wanted []string) error {
	shown := filterSourcesByKind(sources, wanted)
	if len(shown) == 0 {
		if len(sources) == 0 {
			_, err := fmt.Fprintln(out, "No recordable sources reported by the device.")
			return err
		}
		_, err := fmt.Fprintf(out, "No sources of kind %s. Kinds present on this device: %s.\n",
			strings.Join(quoteAll(wanted), ", "), strings.Join(presentSourceKinds(sources), ", "))
		return err
	}

	requested := make(map[string]bool, len(wanted))
	for _, kind := range wanted {
		requested[kind] = true
	}
	total := map[string]int{}
	for _, s := range shown {
		total[strings.ToLower(s.GetKind())]++
	}

	var rows []*agentpbv2.DataSource
	omitted := map[string]int{}
	var floodedOrder []string
	seen := map[string]int{}
	for _, s := range shown {
		kind := strings.ToLower(s.GetKind())
		if !requested[kind] && total[kind] > sourceKindFloodLimit && seen[kind] >= sourceKindFloodLimit {
			if omitted[kind] == 0 {
				floodedOrder = append(floodedOrder, kind)
			}
			omitted[kind]++
			continue
		}
		seen[kind]++
		rows = append(rows, s)
	}

	detailColumn := false
	for _, s := range rows {
		if s.GetDetail() != "" {
			detailColumn = true
			break
		}
	}

	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	header := "SOURCE\tKIND\tCLOCK\tSTATUS"
	if detailColumn {
		header += "\tDETAIL"
	}
	fmt.Fprintln(w, header)
	for _, s := range rows {
		health := "unhealthy"
		if s.GetHealthy() {
			health = "healthy"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s", s.GetId(), s.GetKind(), s.GetClockDomain(), health)
		if detailColumn {
			fmt.Fprintf(w, "\t%s", truncateSourceDetail(s.GetDetail()))
		}
		fmt.Fprintln(w)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// The notes sit outside the tabwriter block; a tab-free line inside it
	// would split the table into two independently aligned halves.
	for _, kind := range floodedOrder {
		if _, err := fmt.Fprintf(out, "\n... %d more %s %s not listed (--kind %s to list all, --json for everything)\n",
			omitted[kind], kind, pluralSource(omitted[kind]), kind); err != nil {
			return err
		}
	}
	return nil
}

func pluralSource(n int) string {
	if n == 1 {
		return "source"
	}
	return "sources"
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

func newDataRecordCmd() *cobra.Command {
	var name string
	var sources, exclude, calibration []string
	var require time.Duration
	c := &cobra.Command{Use: "record", Short: "Start a detached device-local episode", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var cals []*agentpbv2.DataCalibration
		for _, v := range calibration {
			source, path, ok := strings.Cut(v, "=")
			if !ok || source == "" || path == "" {
				return fmt.Errorf("invalid --calibration %q; want source=local-path", v)
			}
			b, e := os.ReadFile(path)
			if e != nil {
				return fmt.Errorf("reading calibration %s: %w", path, e)
			}
			cals = append(cals, &agentpbv2.DataCalibration{Source: source, Contents: b, Filename: filepath.Base(path)})
		}
		return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
			e, e2 := client.Start(cmd.Context(), &agentpbv2.DataStartRequest{Name: name, Sources: sources, ExcludeSources: exclude, RequireUtcUncertaintyNanos: require.Nanoseconds(), Calibrations: cals})
			if e2 != nil {
				return e2
			}
			return printEpisode(cmd, e)
		})
	}}
	c.Flags().StringVar(&name, "name", "", "Human-readable episode name")
	c.Flags().StringArrayVar(&sources, "source", nil, "Source to record (repeatable)")
	c.Flags().StringArrayVar(&exclude, "exclude-source", nil, "Default source to exclude (repeatable)")
	c.Flags().DurationVar(&require, "require-utc-uncertainty", 0, "Fail unless fresh UTC uncertainty is within this bound")
	c.Flags().StringArrayVar(&calibration, "calibration", nil, "Attach source calibration as source=local-path (repeatable)")
	return c
}

func newDataStopCmd() *cobra.Command {
	return &cobra.Command{Use: "stop", Short: "Stop and seal the active episode", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withDataClient(cmd.Context(), func(c agentpbv2.DataServiceClient) error {
			e, err := c.Stop(cmd.Context(), &agentpbv2.DataStopRequest{})
			if err != nil {
				return err
			}
			return printEpisode(cmd, e)
		})
	}}
}

func newDataEpisodesCmd() *cobra.Command {
	return &cobra.Command{Use: "episodes", Short: "List finalized episodes", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withDataClient(cmd.Context(), func(c agentpbv2.DataServiceClient) error {
			r, e := c.Episodes(cmd.Context(), &agentpbv2.DataEpisodesRequest{})
			if e != nil {
				return e
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(r)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "EPISODE\tSTATE\tSIZE\tNAME")
			for _, x := range r.GetEpisodes() {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%d\t%s\n", x.GetId(), x.GetState(), x.GetSizeBytes(), x.GetName())
			}
			return nil
		})
	}}
}

func newDataInspectCmd() *cobra.Command {
	return &cobra.Command{Use: "inspect <episode>", Short: "Inspect and verify an episode manifest", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withDataClient(cmd.Context(), func(c agentpbv2.DataServiceClient) error {
			r, e := c.Inspect(cmd.Context(), &agentpbv2.DataInspectRequest{Episode: args[0], Verify: true})
			if e != nil {
				return e
			}
			if jsonOutput {
				if _, e = cmd.OutOrStdout().Write(r.GetManifestJson()); e != nil {
					return e
				}
			} else {
				if e = printInspection(cmd, r.GetManifestJson()); e != nil {
					return e
				}
			}
			if len(r.GetVerificationErrors()) > 0 {
				return fmt.Errorf("episode verification failed: %s", strings.Join(r.GetVerificationErrors(), "; "))
			}
			return nil
		})
	}}
}

type inspectionManifest struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	State             string `json:"state"`
	CanonicalClock    string `json:"canonical_clock"`
	BootID            string `json:"boot_id"`
	SystemClockStatus string `json:"system_clock_status"`
	CollectorVersion  string `json:"collector_version"`
	Trigger           struct {
		Reason       string `json:"reason"`
		CampaignName string `json:"campaign_name"`
		Expression   string `json:"expression"`
	} `json:"trigger"`
	Upload struct {
		State       string `json:"state"`
		Destination string `json:"destination"`
	} `json:"upload"`
	Labeling struct {
		State       string `json:"state"`
		Destination string `json:"destination"`
	} `json:"labeling"`
	UTC []struct {
		OffsetLower int64  `json:"offset_lower_nanos"`
		OffsetUpper int64  `json:"offset_upper_nanos"`
		Uncertainty int64  `json:"uncertainty_nanos"`
		Confidence  string `json:"confidence"`
	} `json:"utc_observations"`
	Roughtime []struct {
		Confidence string            `json:"confidence"`
		Quorum     int               `json:"quorum"`
		Evidence   []json.RawMessage `json:"evidence"`
	} `json:"roughtime_observations"`
	Sources []struct {
		Source struct {
			ID          string `json:"id"`
			ClockDomain string `json:"clock_domain"`
		} `json:"source"`
		Count           uint64  `json:"count"`
		Drops           *uint64 `json:"drops"`
		DropAccounting  string  `json:"drop_accounting"`
		MappingError    *int64  `json:"mapping_error_nanos"`
		Discontinuities uint64  `json:"discontinuities"`
	} `json:"sources"`
}

func printInspection(cmd *cobra.Command, b []byte) error {
	var m inspectionManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Episode: %s", m.ID)
	if m.Name != "" {
		fmt.Fprintf(w, " (%s)", m.Name)
	}
	fmt.Fprintf(w, "\nState: %s\nCanonical clock: %s\nBoot ID: %s\nSystem clock: %s\nCollector: %s\nTrigger: %s", m.State, m.CanonicalClock, m.BootID, m.SystemClockStatus, m.CollectorVersion, m.Trigger.Reason)
	if m.Trigger.CampaignName != "" {
		fmt.Fprintf(w, " (campaign %s", m.Trigger.CampaignName)
		if m.Trigger.Expression != "" {
			fmt.Fprintf(w, "; %s", m.Trigger.Expression)
		}
		fmt.Fprint(w, ")")
	}
	fmt.Fprintf(w, "\nUpload: %s", m.Upload.State)
	if m.Upload.Destination != "" {
		fmt.Fprintf(w, " -> %s", m.Upload.Destination)
	}
	fmt.Fprintf(w, "\nLabeling: %s", m.Labeling.State)
	if m.Labeling.Destination != "" {
		fmt.Fprintf(w, " -> %s", m.Labeling.Destination)
	}
	fmt.Fprintln(w)
	for i, o := range m.UTC {
		label := "start"
		if i == len(m.UTC)-1 && i > 0 {
			label = "end"
		}
		fmt.Fprintf(w, "UTC %s: offset [%d, %d] ns; uncertainty %s; %s\n", label, o.OffsetLower, o.OffsetUpper, time.Duration(o.Uncertainty), o.Confidence)
	}
	for i, o := range m.Roughtime {
		label := "start"
		if i == len(m.Roughtime)-1 && i > 0 {
			label = "end"
		}
		fmt.Fprintf(w, "Roughtime %s: %s; quorum %d; proofs %d\n", label, o.Confidence, o.Quorum, len(o.Evidence))
	}
	if len(m.Sources) > 0 {
		fmt.Fprintln(w, "\nSOURCE\tCLOCK\tCOUNT\tDROPS\tMAPPING ERROR\tDISCONTINUITIES")
		for _, s := range m.Sources {
			drops := "unknown (" + s.DropAccounting + ")"
			if s.Drops != nil {
				if strings.HasPrefix(s.DropAccounting, "exact") {
					drops = fmt.Sprint(*s.Drops)
				} else {
					drops = fmt.Sprintf("%d known (%s)", *s.Drops, s.DropAccounting)
				}
			}
			mapping := "unknown"
			if s.MappingError != nil {
				mapping = time.Duration(*s.MappingError).String()
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%d\n", s.Source.ID, s.Source.ClockDomain, s.Count, drops, mapping, s.Discontinuities)
		}
	}
	return nil
}

type downloadManifest struct {
	Files []struct {
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

func newDataDownloadCmd() *cobra.Command {
	var output string
	c := &cobra.Command{Use: "download <episode>", Short: "Resume and verify an episode download", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		if output == "" {
			output = id
		}
		stage := output + ".partial"
		if _, e := os.Stat(output); e == nil {
			return fmt.Errorf("destination already exists: %s", output)
		}
		return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
			inspect, e := client.Inspect(cmd.Context(), &agentpbv2.DataInspectRequest{Episode: id})
			if e != nil {
				return e
			}
			var mf downloadManifest
			if e = json.Unmarshal(inspect.GetManifestJson(), &mf); e != nil {
				return e
			}
			if e = os.MkdirAll(stage, 0o750); e != nil {
				return e
			}
			for _, meta := range mf.Files {
				if e = downloadOne(cmd.Context(), client, id, stage, meta.Path, meta.Size, meta.SHA256); e != nil {
					return e
				}
			}
			if e = os.WriteFile(filepath.Join(stage, "manifest.json"), inspect.GetManifestJson(), 0o640); e != nil {
				return e
			}
			if e = os.Rename(stage, output); e != nil {
				return e
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Downloaded %s to %s\n", id, output)
			return nil
		})
	}}
	c.Flags().StringVarP(&output, "output", "o", "", "Destination directory (default episode ID)")
	return c
}

// stagedFilePath resolves a manifest-relative path inside the staging
// directory and rejects anything that would land outside it. Both sides are
// made absolute first: filepath.Join cleans its result, so a relative root such
// as "./ep2.partial" would otherwise never match a "./ep2.partial/" prefix and
// every file in the manifest would be refused.
func stagedFilePath(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || filepath.Clean(rel) != rel || strings.HasPrefix(rel, "..") {
		return "", errors.New("server returned unsafe file path")
	}
	base, e := filepath.Abs(root)
	if e != nil {
		return "", e
	}
	p := filepath.Join(base, rel)
	if !strings.HasPrefix(p, base+string(os.PathSeparator)) {
		return "", errors.New("server file path escapes destination")
	}
	return p, nil
}

func downloadOne(ctx context.Context, c agentpbv2.DataServiceClient, id, root, rel string, size int64, wantHash string) error {
	p, e := stagedFilePath(root, rel)
	if e != nil {
		return e
	}
	if e := os.MkdirAll(filepath.Dir(p), 0o750); e != nil {
		return e
	}
	f, e := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o640)
	if e != nil {
		return e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return e
	}
	offset := st.Size()
	if offset > size {
		return fmt.Errorf("staged file %s is larger than manifest", rel)
	}
	if _, e = f.Seek(offset, io.SeekStart); e != nil {
		return e
	}
	stream, e := c.Download(ctx, &agentpbv2.DataDownloadRequest{Episode: id, Path: rel, Offset: offset})
	if e != nil {
		return e
	}
	for {
		chunk, e := stream.Recv()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if chunk.GetOffset() != offset {
			return fmt.Errorf("non-contiguous chunk for %s", rel)
		}
		if len(chunk.GetData()) > 0 {
			n, e := f.Write(chunk.GetData())
			if e != nil {
				return e
			}
			offset += int64(n)
		}
		if chunk.GetEof() {
			break
		}
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if offset != size {
		return fmt.Errorf("downloaded size mismatch for %s", rel)
	}
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		return e
	}
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return e
	}
	if hex.EncodeToString(h.Sum(nil)) != wantHash {
		return fmt.Errorf("checksum mismatch for %s", rel)
	}
	return nil
}

func printEpisode(cmd *cobra.Command, e *agentpbv2.DataEpisode) error {
	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(e)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Episode %s: %s\n", e.GetId(), e.GetState())
	return nil
}
