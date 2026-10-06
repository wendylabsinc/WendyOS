package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
)

var yoloRepositoryRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,95}/[A-Za-z0-9][A-Za-z0-9_.-]{0,95}$`)
var yoloRevisionRE = regexp.MustCompile(`^[a-f0-9]{40}$`)
var yoloRefRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
var yoloFileRE = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,256}\.onnx$`)

func (g *RobotGateway) registerYOLOTools() {
	name := mcpgo.WithString("name", mcpgo.Required(), mcpgo.MinLength(1), mcpgo.MaxLength(48), mcpgo.Description("Detector name. Stored as a chatgpt-yolo-<name> Wendy Data campaign."))
	g.protocol.AddTool(gatewayTool("deploy_yolo_detector", "Start persistent YOLO detection on an explicit device camera using a public Hugging Face model repository. Supports YOLOv8/YOLO11 float32 ONNX raw detection exports with static batch-1 input and embedded class names. Resolves the repository revision to an immutable commit before deployment; never executes repository Python or .pt checkpoints. Activates the selected camera. Each detection records a one-second local episode with manual upload and requests a 128 MiB campaign quota. Only the device-wide storage quota is currently enforced. Each detection emits a Wendy Data notification. Subscribe to wendy.data.notification for ChatGPT delivery. Agent runtime and camera readiness must be inspected after deployment.", mutating(), robotArgument(), name,
		mcpgo.WithString("model_ref", mcpgo.Required(), mcpgo.MaxLength(300), mcpgo.Description("Hugging Face owner/repository, optionally @revision, or https://huggingface.co/owner/repository.")),
		mcpgo.WithString("model_file", mcpgo.MaxLength(256), mcpgo.Description("Relative ONNX path. Optional when the repository has exactly one ONNX file.")),
		mcpgo.WithString("revision", mcpgo.MaxLength(128), mcpgo.Description("Commit SHA, tag or branch. Defaults to main, resolved to an immutable commit.")),
		mcpgo.WithString("source_id", mcpgo.MaxLength(256), mcpgo.Description("Exact healthy Wendy Data camera source ID. Optional only when one camera is available.")),
		mcpgo.WithArray("labels", mcpgo.Items(map[string]any{"type": "string"}), mcpgo.MinItems(1), mcpgo.MaxItems(32), mcpgo.Description("Model class names to detect. Defaults to person.")),
		mcpgo.WithNumber("threshold", mcpgo.Min(0.01), mcpgo.Max(1)),
		mcpgo.WithNumber("rate", mcpgo.Min(0.1), mcpgo.Max(30)),
	), g.withRobot(RobotTriggerScope, func(ctx context.Context, r *GatewayRobot, s *mcpServer, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if !r.AllowCamera || !g.hasScope(ctx, RobotCameraScope) {
			return mcpgo.NewToolResultError("Camera access is not authorized for this device."), nil
		}
		return deployGatewayYOLO(ctx, r, s, req, resolveGatewayYOLOModel)
	}))
	for _, tool := range []string{"inspect_yolo_detector", "stop_yolo_detector"} {
		scope, annotations := RobotEventsScope, readOnly()
		description := "Inspect a ChatGPT-created YOLO campaign, its pinned model, camera sources, inference state and notification errors. Deployment alone does not establish readiness."
		if tool == "stop_yolo_detector" {
			scope, annotations = RobotTriggerScope, mutating()
			description = "Stop inference for a ChatGPT-created YOLO detector on an explicit device. Previously recorded episodes remain on the device."
		}
		g.protocol.AddTool(gatewayTool(tool, description, annotations, robotArgument(), name), g.withRobot(scope, func(ctx context.Context, r *GatewayRobot, s *mcpServer, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			campaignName, err := gatewayYOLOName(req.GetString("name", ""))
			if err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			if s.GetConn().DataService == nil {
				return mcpgo.NewToolResultError("Update this device's agent to enable Wendy Data inference."), nil
			}
			v, err := s.GetConn().DataService.CampaignInspect(ctx, &agentpbv2.DataCampaignInspectRequest{Name: campaignName})
			if err != nil {
				return mcpgo.NewToolResultError("Detector could not be inspected on this device."), nil
			}
			c, err := decodeGatewayYOLO(v)
			if err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			if req.Params.Name == "stop_yolo_detector" {
				enabled := false
				c.Inference.Enabled = &enabled
				raw, err := yaml.Marshal(c)
				if err != nil {
					return nil, err
				}
				v, err = s.GetConn().DataService.CampaignDeploy(ctx, &agentpbv2.DataCampaignDeployRequest{CampaignYaml: raw})
				if err != nil {
					return mcpgo.NewToolResultError("Detector stop was not confirmed. Inspect its state before retrying."), nil
				}
				return okResult(map[string]any{"robot_id": r.ID, "name": req.GetString("name", ""), "campaign": c.Name, "status": "stop_requested", "runtime_stop_verified": false, "revision": v.Revision, "warnings": v.Warnings, "next_step": "Call inspect_yolo_detector until inference_status.state is disabled. The worker and camera subscriptions may still be stopping."}), nil
			}
			return okResult(map[string]any{"robot_id": r.ID, "name": req.GetString("name", ""), "campaign": c.Name, "state": v.State, "plan": json.RawMessage(v.PlanJson), "warnings": v.Warnings}), nil
		}))
	}
}

func gatewayYOLOName(name string) (string, error) {
	if len(name) > 48 || !gatewayIdentifier.MatchString(name) {
		return "", fmt.Errorf("name must use 1..48 letters, numbers, '-' or '_'")
	}
	return "chatgpt-yolo-" + name, nil
}

func decodeGatewayYOLO(v *agentpbv2.DataCampaign) (data.Campaign, error) {
	var c data.Campaign
	if v == nil || len(v.PlanJson) > 1<<20 || json.Unmarshal(v.PlanJson, &c) != nil || c.Inference == nil || c.Inference.Backend != "yolo_onnx" || !strings.HasPrefix(c.Name, "chatgpt-yolo-") || c.Name != v.Name {
		return c, fmt.Errorf("This campaign is not a ChatGPT YOLO detector; no changes were made.")
	}
	return c, nil
}

type gatewayYOLOModel struct {
	Model    string `json:"model"`
	Revision string `json:"revision"`
	File     string `json:"model_file"`
}
type gatewayYOLOResolver func(context.Context, string, string, string) (gatewayYOLOModel, error)

func resolveGatewayYOLOModel(ctx context.Context, ref, revision, file string) (gatewayYOLOModel, error) {
	client := &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return fetchGatewayYOLOModel(ctx, client, ref, revision, file)
}

func fetchGatewayYOLOModel(ctx context.Context, client *http.Client, ref, revision, file string) (gatewayYOLOModel, error) {
	if len(ref) > 300 {
		return gatewayYOLOModel{}, fmt.Errorf("model_ref is too long")
	}
	if strings.HasPrefix(ref, "https://") {
		u, err := url.Parse(ref)
		if err != nil || u.Host != "huggingface.co" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return gatewayYOLOModel{}, fmt.Errorf("model_ref must name a Hugging Face repository")
		}
		ref = strings.Trim(u.Path, "/")
	}
	if repo, rev, ok := strings.Cut(ref, "@"); ok {
		if revision != "" && rev != revision {
			return gatewayYOLOModel{}, fmt.Errorf("Conflicting model revisions")
		}
		ref, revision = repo, rev
	}
	if revision == "" {
		revision = "main"
	}
	if !yoloRepositoryRE.MatchString(ref) || !yoloRefRE.MatchString(revision) || strings.Contains(revision, "..") {
		return gatewayYOLOModel{}, fmt.Errorf("Use a Hugging Face owner/repository and a commit SHA, tag or branch")
	}
	if file != "" && !validGatewayYOLOFile(file) {
		return gatewayYOLOModel{}, fmt.Errorf("model_file must be a relative .onnx path inside the repository")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://huggingface.co/api/models/"+ref+"/revision/"+url.PathEscape(revision), nil)
	if err != nil {
		return gatewayYOLOModel{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return gatewayYOLOModel{}, fmt.Errorf("Hugging Face model metadata is unavailable; no detector was deployed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return gatewayYOLOModel{}, fmt.Errorf("Hugging Face returned HTTP %d; use an accessible public repository and revision", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(body) > 2<<20 {
		return gatewayYOLOModel{}, fmt.Errorf("Invalid Hugging Face model metadata")
	}
	var info struct {
		SHA      string `json:"sha"`
		Siblings []struct {
			Filename string `json:"rfilename"`
		} `json:"siblings"`
	}
	if json.Unmarshal(body, &info) != nil || !yoloRevisionRE.MatchString(info.SHA) || (yoloRevisionRE.MatchString(revision) && info.SHA != revision) {
		return gatewayYOLOModel{}, fmt.Errorf("Hugging Face did not resolve an immutable model commit")
	}
	candidates := []string{}
	for _, s := range info.Siblings {
		if validGatewayYOLOFile(s.Filename) {
			candidates = append(candidates, s.Filename)
		}
	}
	if file == "" {
		if len(candidates) != 1 {
			return gatewayYOLOModel{}, fmt.Errorf("Repository has %d ONNX files; provide model_file for a YOLOv8/YOLO11 detection export", len(candidates))
		}
		file = candidates[0]
	}
	found := false
	for _, candidate := range candidates {
		found = found || candidate == file
	}
	if !found {
		return gatewayYOLOModel{}, fmt.Errorf("model_file is not present at the resolved Hugging Face revision")
	}
	return gatewayYOLOModel{Model: ref, Revision: info.SHA, File: file}, nil
}
func validGatewayYOLOFile(file string) bool {
	return yoloFileRE.MatchString(file) && path.Clean(file) == file && !strings.HasPrefix(file, "/") && !strings.HasPrefix(file, "../")
}

func deployGatewayYOLO(ctx context.Context, r *GatewayRobot, s *mcpServer, req mcpgo.CallToolRequest, resolve gatewayYOLOResolver) (*mcpgo.CallToolResult, error) {
	name, err := gatewayYOLOName(req.GetString("name", ""))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	client := s.GetConn().DataService
	if client == nil {
		return mcpgo.NewToolResultError("Update this device's agent to enable Wendy Data inference."), nil
	}
	existing, err := client.CampaignInspect(ctx, &agentpbv2.DataCampaignInspectRequest{Name: name})
	if err == nil {
		if _, err := decodeGatewayYOLO(existing); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
	} else if status.Code(err) != codes.NotFound {
		return mcpgo.NewToolResultError("Could not check the existing campaign; no detector was deployed."), nil
	}
	sources, err := client.Sources(ctx, &agentpbv2.DataSourcesRequest{})
	if err != nil {
		return mcpgo.NewToolResultError("Camera source inventory is unavailable; no detector was deployed."), nil
	}
	available := []string{}
	for _, source := range sources.Sources {
		if source.Kind == "camera" && source.Healthy {
			available = append(available, source.Id)
		}
	}
	source := req.GetString("source_id", "")
	if source == "" && len(available) == 1 {
		source = available[0]
	}
	found := false
	for _, id := range available {
		found = found || id == source
	}
	if !found {
		return mcpgo.NewToolResultError(fmt.Sprintf("Choose an exact healthy camera source_id from %v; no detector was deployed.", available)), nil
	}
	model, err := resolve(ctx, req.GetString("model_ref", ""), req.GetString("revision", ""), req.GetString("model_file", ""))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	c := data.Campaign{Version: 1, Name: name, Sources: []data.CampaignSource{{Camera: source}}, Inference: &data.CampaignInference{Backend: "yolo_onnx", Model: model.Model, Revision: model.Revision, ModelFile: model.File, Labels: req.GetStringSlice("labels", []string{"person"}), Threshold: req.GetFloat("threshold", 0.5), Rate: req.GetFloat("rate", 1), Event: name + ".detected", ClearAfter: "5s", Cooldown: "30s"}, Capture: data.CampaignCapture{Buffer: "0s", AfterTrigger: "1s", Triggers: []data.CampaignTrigger{{Event: name + ".detected"}}}, Upload: data.CampaignUpload{When: "manual"}, Retention: data.CampaignRetention{LocalQuota: "128MiB"}, Export: data.CampaignExport{Annotation: "cvat"}, Notify: &data.CampaignNotify{On: data.NotifyOnDetection}}
	raw, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	if _, err = data.ParseCampaign(raw); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	deployed, err := client.CampaignDeploy(ctx, &agentpbv2.DataCampaignDeployRequest{CampaignYaml: raw})
	if err != nil {
		return mcpgo.NewToolResultError("YOLO deployment was not confirmed. The agent must support yolo_onnx inference. Inspect the detector before retrying."), nil
	}
	return okResult(map[string]any{"robot_id": r.ID, "name": req.GetString("name", ""), "campaign": name, "model": model, "source_id": source, "state": deployed.State, "revision": deployed.Revision, "warnings": deployed.Warnings, "readiness": "not_checked", "next_step": "Call inspect_yolo_detector to check model loading and camera state. Subscribe to wendy.data.notification with the returned robot_id and campaign to receive detections.", "notification": map[string]any{"name": "wendy.data.notification", "arguments": map[string]any{"robot_id": r.ID, "campaign": name}}, "capture": map[string]any{"seconds": 1, "requested_local_quota": "128MiB", "local_quota_enforced": false, "enforced_quota": "device_wide", "upload": "manual"}}), nil
}
