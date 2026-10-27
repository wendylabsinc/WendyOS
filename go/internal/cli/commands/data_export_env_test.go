package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeExportEnvData is a DataServiceClient that answers only the three RPCs
// export-env needs. The embedded interface leaves every other method nil, so an
// unexpected call panics rather than silently returning a zero value.
type fakeExportEnvData struct {
	agentpbv2.DataServiceClient
	campaigns []*agentpbv2.DataCampaign
	inspect   map[string]*agentpbv2.DataCampaign
	sources   []*agentpbv2.DataSource
}

func (f *fakeExportEnvData) Campaigns(context.Context, *agentpbv2.DataCampaignsRequest, ...grpc.CallOption) (*agentpbv2.DataCampaignsResponse, error) {
	return &agentpbv2.DataCampaignsResponse{Campaigns: f.campaigns}, nil
}

func (f *fakeExportEnvData) CampaignInspect(_ context.Context, in *agentpbv2.DataCampaignInspectRequest, _ ...grpc.CallOption) (*agentpbv2.DataCampaign, error) {
	if campaign, ok := f.inspect[in.GetName()]; ok {
		return campaign, nil
	}
	return nil, status.Errorf(codes.NotFound, "no campaign %q", in.GetName())
}

func (f *fakeExportEnvData) Sources(context.Context, *agentpbv2.DataSourcesRequest, ...grpc.CallOption) (*agentpbv2.DataSourcesResponse, error) {
	return &agentpbv2.DataSourcesResponse{Sources: f.sources}, nil
}

// fakeExportEnvVideo is a WendyVideoServiceClient that answers only
// ListVideoDevices.
type fakeExportEnvVideo struct {
	agentpb.WendyVideoServiceClient
	devices []*agentpb.VideoDevice
}

func (f *fakeExportEnvVideo) ListVideoDevices(context.Context, *agentpb.ListVideoDevicesRequest, ...grpc.CallOption) (*agentpb.ListVideoDevicesResponse, error) {
	return &agentpb.ListVideoDevicesResponse{Devices: f.devices}, nil
}

// trainingCampaign builds a deployed campaign with a camera source and a
// training action, and encodes it the way the agent does: json.MarshalIndent of
// the data.Campaign into DataCampaign.plan_json.
func trainingCampaign(t *testing.T, name string) *agentpbv2.DataCampaign {
	t.Helper()
	campaign := data.Campaign{
		Version: 1,
		Name:    name,
		Sources: []data.CampaignSource{{
			Camera:  "front",
			Capture: &data.SourceCapture{MaxResolution: "1280x720", Rate: 15},
		}},
		Training: &data.CampaignTraining{
			RateHz: 20,
			Action: data.CampaignTrainingAction{
				Stream:   "cmd",
				Channels: []string{"wheel_l", "wheel_r"},
				Range:    [][]float64{{-1, 1}, {-1, 1}},
				Unit:     data.TrainingUnit{"m/s"},
			},
		},
		State:    "armed",
		Revision: "abc123def4567890",
	}
	planJSON, err := json.Marshal(campaign)
	if err != nil {
		t.Fatalf("marshal campaign: %v", err)
	}
	return &agentpbv2.DataCampaign{Name: name, State: "armed", Revision: campaign.Revision, PlanJson: planJSON}
}

// nonTrainingCampaign is a deployed campaign with no training block.
func nonTrainingCampaign(t *testing.T, name string) *agentpbv2.DataCampaign {
	t.Helper()
	campaign := data.Campaign{
		Version:  1,
		Name:     name,
		Sources:  []data.CampaignSource{{Camera: "front"}},
		State:    "armed",
		Revision: "0011223344556677",
	}
	planJSON, err := json.Marshal(campaign)
	if err != nil {
		t.Fatalf("marshal campaign: %v", err)
	}
	return &agentpbv2.DataCampaign{Name: name, State: "armed", Revision: campaign.Revision, PlanJson: planJSON}
}

func cameraDevice() (*fakeExportEnvData, *fakeExportEnvVideo, *agentpbv2.DataCampaign) {
	sources := []*agentpbv2.DataSource{
		{Id: "v4l2:/dev/video0", Kind: "camera", Healthy: true, Detail: "front cam"},
	}
	video := &fakeExportEnvVideo{devices: []*agentpb.VideoDevice{
		{Id: 0, Name: "front cam", Path: "/dev/video0", PixelFormats: []string{"yuyv422"}},
	}}
	return &fakeExportEnvData{sources: sources}, video, nil
}

func TestRunDataExportEnvWritesDirectory(t *testing.T) {
	dir := t.TempDir()
	campaign := trainingCampaign(t, "pick_place")
	dataClient, video, _ := cameraDevice()
	dataClient.inspect = map[string]*agentpbv2.DataCampaign{"pick_place": campaign}

	var out, errOut bytes.Buffer
	dest := filepath.Join(dir, "pick_place-env")
	err := runDataExportEnv(context.Background(), dataClient, video, exportEnvRequest{
		campaign: "pick_place",
		output:   dest,
	}, &out, &errOut)
	if err != nil {
		t.Fatalf("runDataExportEnv: %v", err)
	}

	// env.json parses and matches the campaign.
	envBytes, err := os.ReadFile(filepath.Join(dest, "env.json"))
	if err != nil {
		t.Fatalf("reading env.json: %v", err)
	}
	var env struct {
		Campaign      string  `json:"campaign"`
		Revision      string  `json:"revision"`
		Profile       string  `json:"profile"`
		ControlRateHz float64 `json:"control_rate_hz"`
		Observations  []struct {
			Kind        string `json:"kind"`
			PixelFormat string `json:"pixel_format"`
			Dims        []int  `json:"dims"`
		} `json:"observations"`
		Action *struct {
			Stream   string    `json:"stream"`
			Channels []string  `json:"channels"`
			Low      []float64 `json:"low"`
			High     []float64 `json:"high"`
			Unit     string    `json:"unit"`
		} `json:"action"`
	}
	if err := json.Unmarshal(envBytes, &env); err != nil {
		t.Fatalf("env.json is not valid JSON: %v", err)
	}
	if env.Campaign != "pick_place" {
		t.Fatalf("env.json campaign = %q, want pick_place", env.Campaign)
	}
	if env.Revision != campaign.GetRevision() {
		t.Fatalf("env.json revision = %q, want %q", env.Revision, campaign.GetRevision())
	}
	if env.Profile != "custom" {
		t.Fatalf("env.json profile = %q, want custom", env.Profile)
	}
	if env.ControlRateHz != 20 {
		t.Fatalf("env.json control_rate_hz = %v, want 20", env.ControlRateHz)
	}
	if len(env.Observations) != 1 || env.Observations[0].Kind != "image" {
		t.Fatalf("env.json observations = %+v, want one image observation", env.Observations)
	}
	if got := env.Observations[0].Dims; len(got) != 3 || got[0] != 720 || got[1] != 1280 {
		t.Fatalf("image dims = %v, want [720 1280 3]", got)
	}
	if env.Observations[0].PixelFormat != "yuyv422" {
		t.Fatalf("pixel format = %q, want yuyv422 from the matched video device", env.Observations[0].PixelFormat)
	}
	if env.Action == nil {
		t.Fatal("env.json has no action; the training action was dropped")
	}
	if env.Action.Stream != "cmd" || len(env.Action.Channels) != 2 {
		t.Fatalf("action = %+v, want stream cmd with two channels", env.Action)
	}
	if len(env.Action.Low) != 2 || env.Action.Low[0] != -1 || env.Action.High[0] != 1 {
		t.Fatalf("action range split wrong: low=%v high=%v", env.Action.Low, env.Action.High)
	}
	if env.Action.Unit != "m/s" {
		t.Fatalf("action unit = %q, want m/s", env.Action.Unit)
	}

	// scene.xml is non-empty, well-formed XML.
	sceneBytes, err := os.ReadFile(filepath.Join(dest, "scene.xml"))
	if err != nil {
		t.Fatalf("reading scene.xml: %v", err)
	}
	if len(bytes.TrimSpace(sceneBytes)) == 0 {
		t.Fatal("scene.xml is empty")
	}
	decoder := xml.NewDecoder(bytes.NewReader(sceneBytes))
	for {
		_, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("scene.xml is not well-formed XML: %v", err)
		}
	}

	// README.md exists.
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("README.md missing: %v", err)
	}
}

func TestRunDataExportEnvAutoPicksSoleCampaign(t *testing.T) {
	dir := t.TempDir()
	campaign := trainingCampaign(t, "solo")
	dataClient, video, _ := cameraDevice()
	dataClient.campaigns = []*agentpbv2.DataCampaign{{Name: "solo", State: "armed", Revision: campaign.GetRevision()}}
	dataClient.inspect = map[string]*agentpbv2.DataCampaign{"solo": campaign}

	var out, errOut bytes.Buffer
	dest := filepath.Join(dir, "solo-env")
	err := runDataExportEnv(context.Background(), dataClient, video, exportEnvRequest{output: dest}, &out, &errOut)
	if err != nil {
		t.Fatalf("runDataExportEnv auto-pick: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "env.json")); err != nil {
		t.Fatalf("auto-pick did not write env.json: %v", err)
	}
}

func TestRunDataExportEnvRefusesSeveralCampaigns(t *testing.T) {
	dataClient, video, _ := cameraDevice()
	dataClient.campaigns = []*agentpbv2.DataCampaign{
		{Name: "beta"},
		{Name: "alpha"},
	}

	var out, errOut bytes.Buffer
	err := runDataExportEnv(context.Background(), dataClient, video, exportEnvRequest{}, &out, &errOut)
	if err == nil {
		t.Fatal("expected a refusal when several campaigns are deployed")
	}
	if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "beta") {
		t.Fatalf("error does not name the deployed campaigns: %v", err)
	}
}

func TestRunDataExportEnvRejectsNoTrainingBlock(t *testing.T) {
	dir := t.TempDir()
	dataClient, video, _ := cameraDevice()
	dataClient.inspect = map[string]*agentpbv2.DataCampaign{"plain": nonTrainingCampaign(t, "plain")}

	var out, errOut bytes.Buffer
	err := runDataExportEnv(context.Background(), dataClient, video, exportEnvRequest{
		campaign: "plain",
		output:   filepath.Join(dir, "plain-env"),
	}, &out, &errOut)
	if err == nil {
		t.Fatal("expected an error for a campaign with no training block")
	}
	if !strings.Contains(err.Error(), "no training block") {
		t.Fatalf("error does not explain the missing training block: %v", err)
	}
}

func TestRunDataExportEnvRefusesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "taken-env")
	if err := os.MkdirAll(dest, 0o750); err != nil {
		t.Fatal(err)
	}
	dataClient, video, _ := cameraDevice()
	dataClient.inspect = map[string]*agentpbv2.DataCampaign{"taken": trainingCampaign(t, "taken")}

	var out, errOut bytes.Buffer
	err := runDataExportEnv(context.Background(), dataClient, video, exportEnvRequest{
		campaign: "taken",
		output:   dest,
	}, &out, &errOut)
	if err == nil {
		t.Fatal("expected a refusal when the destination already exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error does not report the existing destination: %v", err)
	}
}
