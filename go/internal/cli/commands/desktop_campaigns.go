package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"slices"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"gopkg.in/yaml.v3"
)

// The native editor intentionally offers a bounded, validated subset of v1.
// YAML and wire plan_json stay behind this typed API.
type DesktopCampaignInput struct {
	Model, ModelRevision         string
	Labels                       []string
	Audio, ROS2                  []string
	Telemetry                    bool
	Classification               bool
	Name, Camera, Event          string
	RunInference, Notify, Upload bool
	Threshold                    float64
	Cooldown, Before, After      uint32
}
type DesktopCampaign struct {
	Name, State, Revision, Summary, InferenceState, Problem string
	Warnings                                                []string
}

func DesktopCampaignYAML(in DesktopCampaignInput) ([]byte, error) {
	if in.Classification && (!in.RunInference || in.Model == "") {
		return nil, errors.New("choose an image-classification model")
	}
	if strings.TrimSpace(in.Camera) == "" && (in.RunInference || (len(in.Audio) == 0 && len(in.ROS2) == 0 && !in.Telemetry)) {
		return nil, errors.New("choose a camera")
	}
	if in.Before > 300 || in.After == 0 || in.After > 86400 {
		return nil, errors.New("clip duration is outside the supported range")
	}
	event := strings.TrimSpace(in.Event)
	if in.RunInference && event == "" {
		event = "person_detected"
	}
	if event == "" {
		return nil, errors.New("enter the event emitted by your application")
	}
	plan := data.Campaign{Version: 1, Name: strings.TrimSpace(in.Name),
		Sources: nil,
		Capture: data.CampaignCapture{Buffer: fmt.Sprintf("%ds", in.Before), AfterTrigger: fmt.Sprintf("%ds", in.After), Triggers: []data.CampaignTrigger{{Event: event}}},
		Upload:  data.CampaignUpload{When: "manual"}, Export: data.CampaignExport{Annotation: "none"}}
	if in.Camera != "" {
		plan.Sources = append(plan.Sources, data.CampaignSource{Camera: in.Camera, Capture: &data.SourceCapture{Mode: "continuous"}})
	}
	for _, id := range in.Audio {
		if strings.TrimSpace(id) == "" {
			return nil, errors.New("choose an audio source")
		}
		plan.Sources = append(plan.Sources, data.CampaignSource{Audio: id})
	}
	for _, id := range in.ROS2 {
		if strings.TrimSpace(id) == "" {
			return nil, errors.New("choose a ROS 2 source")
		}
		plan.Sources = append(plan.Sources, data.CampaignSource{ROS2: id})
	}
	if in.Telemetry {
		plan.Sources = append(plan.Sources, data.CampaignSource{Telemetry: true})
	}
	if in.Upload {
		plan.Upload.When = "always"
	}
	if in.Notify {
		plan.Notify = &data.CampaignNotify{On: data.NotifyOnEvent, Event: event}
	}
	if in.RunInference {
		model, revision, labels := in.Model, in.ModelRevision, in.Labels
		if model == "" {
			model = "facebook/detr-resnet-50"
			revision = "1d5f47bd3bdd2c4bbfa585418ffe6da5028b4c0b"
			labels = []string{"person"}
		}
		task := ""
		if in.Classification {
			task = "image-classification"
		}
		plan.Inference = &data.CampaignInference{Task: task, Model: model, Revision: revision,
			Labels: labels, Threshold: in.Threshold, Rate: 1, Event: event, ClearAfter: "5s", Cooldown: fmt.Sprintf("%ds", in.Cooldown)}
	}
	raw, err := yaml.Marshal(plan)
	if err != nil {
		return nil, err
	}
	_, err = data.ParseCampaign(raw)
	return raw, err
}

func desktopCampaign(p *pb.DataCampaign) DesktopCampaign {
	out := DesktopCampaign{Name: p.GetName(), State: p.GetState(), Revision: p.GetRevision(), Warnings: p.GetWarnings()}
	var plan data.Campaign
	if err := json.Unmarshal(p.GetPlanJson(), &plan); err != nil {
		out.Problem = "The Agent did not return a readable campaign plan."
		return out
	}
	var sources []string
	for _, s := range plan.Sources {
		if s.Audio != "" {
			sources = append(sources, s.Audio)
		}
		if s.ROS2 != "" {
			sources = append(sources, s.ROS2)
		}
		if s.Telemetry {
			sources = append(sources, "Device telemetry")
		}
		if s.Camera != "" {
			name := s.Camera
			if name == "*" {
				name = "Any camera"
			}
			sources = append(sources, name)
		}
	}
	trigger := "Event trigger"
	if plan.Inference != nil {
		trigger = "Detect " + strings.Join(plan.Inference.Labels, ", ")
	} else if len(plan.Capture.Triggers) > 0 {
		trigger = plan.Capture.Triggers[0].Event
	}
	action := "Record a clip"
	if plan.Notify != nil {
		action = "Notify and record"
	}
	out.Summary = strings.Join(sources, ", ") + " · " + trigger + " · " + action
	if s := plan.InferenceStatus; s != nil {
		out.InferenceState = s.State
		out.Problem = s.Error
		if s.NotificationError != "" {
			out.Problem += "\nNotification: " + s.NotificationError
		}
	}
	return out
}

func DesktopCampaigns(ctx context.Context, conn *grpcclient.AgentConnection) ([]DesktopCampaign, error) {
	r, err := conn.DataService.Campaigns(ctx, &pb.DataCampaignsRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]DesktopCampaign, 0, len(r.GetCampaigns()))
	for _, p := range r.GetCampaigns() {
		out = append(out, desktopCampaign(p))
	}
	return out, nil
}

func DesktopDeployCampaign(ctx context.Context, conn *grpcclient.AgentConnection, in DesktopCampaignInput) error {
	if in.Classification {
		info, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
		if err != nil {
			return err
		}
		if !slices.Contains(info.GetFeatureset(), "data-inference-classification") {
			return errors.New("image classification requires an updated Wendy Agent on this device; object detection is available on the current Agent")
		}
	}
	raw, err := DesktopCampaignYAML(in)
	if err != nil {
		return err
	}
	// Creating a new rule must not silently replace an existing campaign.
	current, err := conn.DataService.Campaigns(ctx, &pb.DataCampaignsRequest{})
	if err != nil {
		return err
	}
	for _, p := range current.GetCampaigns() {
		if p.GetName() == strings.TrimSpace(in.Name) {
			return errors.New("a campaign with this name already exists; choose a different name")
		}
	}
	if err := registerCloudApps(ctx, conn, []string{data.CampaignAppID(strings.TrimSpace(in.Name))}, false); err != nil {
		return err
	}
	_, err = conn.DataService.CampaignDeploy(ctx, &pb.DataCampaignDeployRequest{CampaignYaml: raw})
	return err
}
