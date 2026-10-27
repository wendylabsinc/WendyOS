package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/simenv"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// dataExportEnvResult is the --json payload of "wendy data export-env". Like
// "wendy data download" it reports what it wrote to the local filesystem, which
// no wire message describes, so it is a command-shaped struct rather than a
// protocol buffer message. Files are the base names written under Destination.
type dataExportEnvResult struct {
	Campaign    string   `json:"campaign"`
	Revision    string   `json:"revision,omitempty"`
	Profile     string   `json:"profile"`
	Destination string   `json:"destination"`
	Files       []string `json:"files"`
	Warnings    []string `json:"warnings,omitempty"`
}

// exportEnvRequest carries the resolved command inputs into runDataExportEnv so
// the translation and file-writing logic is testable without a live device.
type exportEnvRequest struct {
	campaign string
	output   string
	profile  string
	json     bool
}

func newDataExportEnvCmd() *cobra.Command {
	var output, profile string
	c := &cobra.Command{
		Use:   "export-env <campaign>",
		Short: "Translate a training campaign into a simulator-environment directory",
		Long: "Read a deployed training campaign, resolve its sources against the device, and write a\n" +
			"simulator-environment directory holding env.json (the interface manifest), scene.xml\n" +
			"(a MuJoCo scene scaffold), and a README.md. Without a campaign name, the sole deployed\n" +
			"campaign is used when there is exactly one.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			conn, err := connectToAgent(cmd.Context())
			if err != nil {
				return err
			}
			defer conn.Close()
			return runDataExportEnv(cmd.Context(), conn.DataService, conn.VideoService, exportEnvRequest{
				campaign: name,
				output:   output,
				profile:  profile,
				json:     jsonOutput,
			}, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	c.Flags().StringVarP(&output, "output", "o", "", "Destination directory (default ./<campaign>-env)")
	c.Flags().StringVar(&profile, "profile", simenv.ProfileCustom, "Robot profile: go2, g1, so101, rosmaster-r2, or custom")
	return c
}

// runDataExportEnv resolves the campaign, translates it through the pure simenv
// package, and writes the environment directory. It takes the two service
// clients directly rather than a connection so a test can drive it with fakes.
func runDataExportEnv(ctx context.Context, dataClient agentpbv2.DataServiceClient, videoClient agentpb.WendyVideoServiceClient, req exportEnvRequest, out, errOut io.Writer) error {
	name, err := resolveExportEnvCampaign(ctx, dataClient, req.campaign)
	if err != nil {
		return err
	}

	inspected, err := dataClient.CampaignInspect(ctx, &agentpbv2.DataCampaignInspectRequest{Name: name})
	if err != nil {
		return err
	}
	// plan_json is json.MarshalIndent of the agent's data.Campaign (see
	// campaignMessage in internal/agent/services/data_service.go), so the JSON
	// unmarshal is its exact inverse. data.ParseCampaign is deliberately not
	// used: it is a YAML decoder with KnownFields(true), and the plan carries
	// device-populated keys (state, revision, deployed_unix_nanos, warnings)
	// whose struct tags are yaml:"-", which that decoder would reject.
	var campaign data.Campaign
	if err := json.Unmarshal(inspected.GetPlanJson(), &campaign); err != nil {
		return fmt.Errorf("parsing campaign plan for %q: %w", name, err)
	}
	if campaign.Training == nil {
		return fmt.Errorf("campaign %s has no training block; add a training.action stream to export an environment", name)
	}

	sourcesResp, err := dataClient.Sources(ctx, &agentpbv2.DataSourcesRequest{})
	if err != nil {
		return err
	}
	videoResp, err := videoClient.ListVideoDevices(ctx, &agentpb.ListVideoDevicesRequest{})
	if err != nil {
		return err
	}

	profile := req.profile
	if profile == "" {
		profile = simenv.ProfileCustom
	}

	env, err := simenv.Resolve(simenv.Input{
		Campaign:      campaign.Name,
		Revision:      inspected.GetRevision(),
		Sources:       campaign.Sources,
		ControlRateHz: campaign.Training.RateHz,
		ActionSpec:    trainingActionToSimenv(campaign.Training),
		DataSources:   sourcesResp.GetSources(),
		VideoDevices:  videoResp.GetDevices(),
		Profile:       profile,
	})
	if err != nil {
		return err
	}

	envJSON, err := simenv.RenderEnvJSON(env)
	if err != nil {
		return err
	}
	sceneXML, err := simenv.RenderMJCF(env)
	if err != nil {
		return err
	}

	destination, stage := exportEnvDestination(campaign.Name, req.output)
	if _, statErr := os.Stat(destination); statErr == nil {
		return fmt.Errorf("destination already exists: %s", destination)
	}
	if err := os.MkdirAll(stage, 0o750); err != nil {
		return err
	}

	outputs := []struct {
		name     string
		contents []byte
	}{
		{"env.json", envJSON},
		{"scene.xml", sceneXML},
		{"README.md", exportEnvReadme(env)},
	}
	written := make([]string, 0, len(outputs))
	for _, file := range outputs {
		if err := atomicfile.Write(filepath.Join(stage, file.name), file.contents, 0o640); err != nil {
			return err
		}
		written = append(written, file.name)
	}
	if err := os.Rename(stage, destination); err != nil {
		return err
	}

	for _, warning := range env.Warnings {
		fmt.Fprintf(errOut, "Warning: %s\n", warning)
	}

	if req.json {
		return json.NewEncoder(out).Encode(dataExportEnvResult{
			Campaign:    env.Campaign,
			Revision:    env.Revision,
			Profile:     env.Profile,
			Destination: destination,
			Files:       written,
			Warnings:    env.Warnings,
		})
	}

	fmt.Fprintf(out, "Exported %s environment for %s to %s\n", env.Profile, env.Campaign, destination)
	for _, file := range written {
		fmt.Fprintf(out, "  %s\n", file)
	}
	return nil
}

// resolveExportEnvCampaign returns the campaign to export. A named campaign is
// taken as given; otherwise the sole deployed campaign is used, and zero or
// several deployed campaigns are a clear error naming what is present.
func resolveExportEnvCampaign(ctx context.Context, client agentpbv2.DataServiceClient, requested string) (string, error) {
	if strings.TrimSpace(requested) != "" {
		return requested, nil
	}
	response, err := client.Campaigns(ctx, &agentpbv2.DataCampaignsRequest{})
	if err != nil {
		return "", err
	}
	campaigns := response.GetCampaigns()
	const usage = "name a campaign, for example: wendy data export-env <campaign>"
	switch len(campaigns) {
	case 0:
		return "", fmt.Errorf("no campaigns are deployed on this device; %s", usage)
	case 1:
		return campaigns[0].GetName(), nil
	default:
		names := make([]string, 0, len(campaigns))
		for _, campaign := range campaigns {
			names = append(names, campaign.GetName())
		}
		sort.Strings(names)
		return "", fmt.Errorf("several campaigns are deployed, so one must be named: %s; %s", strings.Join(names, ", "), usage)
	}
}

// trainingActionToSimenv maps a campaign training action onto the simenv action
// spec: the per-channel [low, high] range pairs are split into aligned Low and
// High slices, and the one-or-per-channel unit labels are joined into the single
// unit string simenv carries.
func trainingActionToSimenv(training *data.CampaignTraining) *simenv.Action {
	if training == nil {
		return nil
	}
	action := training.Action
	low := make([]float64, len(action.Range))
	high := make([]float64, len(action.Range))
	for i, bounds := range action.Range {
		if len(bounds) == 2 {
			low[i], high[i] = bounds[0], bounds[1]
		}
	}
	return &simenv.Action{
		Stream:   action.Stream,
		Channels: action.Channels,
		Low:      low,
		High:     high,
		Unit:     strings.Join(action.Unit, ", "),
	}
}

// exportEnvDestination resolves the output directory and the staging directory
// renamed onto it once every file is written. It mirrors downloadDestination:
// filepath.Clean strips a trailing separator so the staging directory is a
// sibling of the destination rather than a child of it.
func exportEnvDestination(campaign, output string) (destination, stage string) {
	if output == "" {
		output = campaign + "-env"
	}
	destination = filepath.Clean(output)
	return destination, destination + ".partial"
}

// exportEnvReadme renders the one-paragraph README that ships beside the
// generated environment.
func exportEnvReadme(env simenv.Env) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# Simulator environment for %s\n\n", env.Campaign)
	fmt.Fprintf(&b, "Generated by `wendy data export-env` from campaign `%s`", env.Campaign)
	if env.Revision != "" {
		fmt.Fprintf(&b, " at revision `%s`", shortRevision(env.Revision))
	}
	fmt.Fprintf(&b, " for the `%s` profile. ", env.Profile)
	b.WriteString("`env.json` is the interface manifest listing the simulated observations and the action a policy drives; ")
	b.WriteString("`scene.xml` is a MuJoCo (MJCF) scene scaffold meant to sit beside the matching simulator model under `go/simulator/<kind>/`. ")
	b.WriteString("Both are a scaffold, not a validated world, so review them before use; any dimensions that could not be derived from the campaign are recorded under `warnings` in `env.json`.\n")
	return []byte(b.String())
}
