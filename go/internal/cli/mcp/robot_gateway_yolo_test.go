package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type yoloTestTransport func(*http.Request) (*http.Response, error)

func (f yoloTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGatewayYOLOModelResolution(t *testing.T) {
	sha := strings.Repeat("a", 40)
	calls := 0
	client := &http.Client{Transport: yoloTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://huggingface.co/api/models/acme/yolo/revision/main" {
			t.Fatalf("unexpected model metadata URL %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"sha":"` + sha + `","siblings":[{"rfilename":"model.onnx"},{"rfilename":"model.pt"}]}`)), Header: http.Header{}}, nil
	})}
	for _, ref := range []string{"acme/yolo", "acme/yolo@main", "https://huggingface.co/acme/yolo"} {
		model, err := fetchGatewayYOLOModel(context.Background(), client, ref, "", "")
		if err != nil || model.Revision != sha || model.File != "model.onnx" || model.Model != "acme/yolo" {
			t.Fatalf("model=%+v err=%v", model, err)
		}
	}
	if calls != 3 {
		t.Fatal(calls)
	}
	for _, ref := range []string{"https://evil.example/acme/yolo", "https://huggingface.co@evil.example/acme/yolo", "../model", "acme/yolo/../../private", "file:///tmp/model.onnx", "https://huggingface.co/acme/yolo?token=x"} {
		if _, err := fetchGatewayYOLOModel(context.Background(), client, ref, "", ""); err == nil {
			t.Fatalf("accepted %q", ref)
		}
	}
	if calls != 3 {
		t.Fatal("invalid model ref made a network request")
	}
	for _, file := range []string{"../secret.onnx", "/secret.onnx", "model.pt", "models/../secret.onnx"} {
		if _, err := fetchGatewayYOLOModel(context.Background(), client, "acme/yolo", "", file); err == nil {
			t.Fatalf("accepted %q", file)
		}
	}
	if calls != 3 {
		t.Fatal("invalid model file made a network request")
	}
}

func TestGatewayYOLOAmbiguityAndMissingPin(t *testing.T) {
	for _, body := range []string{
		`{"sha":"main","siblings":[{"rfilename":"model.onnx"}]}`,
		`{"sha":"` + strings.Repeat("a", 40) + `","siblings":[{"rfilename":"one.onnx"},{"rfilename":"two.onnx"}]}`,
		`{"sha":"` + strings.Repeat("a", 40) + `","siblings":[{"rfilename":"model.pt"}]}`,
	} {
		client := &http.Client{Transport: yoloTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		if _, err := fetchGatewayYOLOModel(context.Background(), client, "acme/yolo", "", ""); err == nil {
			t.Fatal("accepted ambiguous or unpinned model", body)
		}
	}
}

type yoloDataClient struct {
	agentpbv2.DataServiceClient
	sources  []*agentpbv2.DataSource
	existing *agentpbv2.DataCampaign
	deployed *data.Campaign
}

func (c *yoloDataClient) CampaignInspect(context.Context, *agentpbv2.DataCampaignInspectRequest, ...grpc.CallOption) (*agentpbv2.DataCampaign, error) {
	if c.existing != nil {
		return c.existing, nil
	}
	return nil, status.Error(codes.NotFound, "no campaign")
}
func (c *yoloDataClient) Sources(context.Context, *agentpbv2.DataSourcesRequest, ...grpc.CallOption) (*agentpbv2.DataSourcesResponse, error) {
	return &agentpbv2.DataSourcesResponse{Sources: c.sources}, nil
}
func (c *yoloDataClient) CampaignDeploy(_ context.Context, r *agentpbv2.DataCampaignDeployRequest, _ ...grpc.CallOption) (*agentpbv2.DataCampaign, error) {
	campaign, err := data.ParseCampaign(r.CampaignYaml)
	if err != nil {
		return nil, err
	}
	c.deployed = &campaign
	b, _ := json.Marshal(campaign)
	return &agentpbv2.DataCampaign{Name: campaign.Name, State: "ARMED", Revision: campaign.Revision, PlanJson: b}, nil
}

func TestGatewayYOLODeployUsesExactCameraAndNotification(t *testing.T) {
	client := &yoloDataClient{sources: []*agentpbv2.DataSource{{Id: "v4l2:/dev/video0", Kind: "camera", Healthy: true}}}
	s := New(&config.Config{}, nil)
	s.SetConn(&grpcclient.AgentConnection{DataService: client})
	resolve := func(context.Context, string, string, string) (gatewayYOLOModel, error) {
		return gatewayYOLOModel{Model: "acme/yolo", Revision: strings.Repeat("a", 40), File: "model.onnx"}, nil
	}
	result, err := deployGatewayYOLO(context.Background(), &GatewayRobot{ID: "alpha"}, s, callToolReq("deploy_yolo_detector", map[string]any{"name": "people", "model_ref": "acme/yolo"}), resolve)
	if err != nil || result.IsError || client.deployed == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	c := client.deployed
	if c.Name != "chatgpt-yolo-people" || c.Inference.Backend != "yolo_onnx" || c.Inference.ModelFile != "model.onnx" || c.Notify.On != data.NotifyOnDetection || c.Notify.Webhook != "" || c.Upload.When != "manual" || c.Sources[0].Camera != "v4l2:/dev/video0" {
		t.Fatalf("bad campaign %+v", c)
	}
	if !c.Inference.IsEnabled() {
		t.Fatal("detector was not enabled")
	}
	body, _ := json.Marshal(result.StructuredContent)
	if !strings.Contains(string(body), `"local_quota_enforced":false`) || !strings.Contains(string(body), `"requested_local_quota":"128MiB"`) {
		t.Fatal("deployment claimed an enforced campaign quota", string(body))
	}
	plan, _ := json.Marshal(c)
	client.existing = &agentpbv2.DataCampaign{Name: c.Name, PlanJson: plan}
	cfg := gatewayTestConfig()
	cfg.StateDirectory = t.TempDir()
	gateway, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{DataService: client}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server := gatewayTestHTTP(t, gateway)
	alice := gatewayHTTPClient(t, server.URL, gatewayTestEnv("ALICE_TOKEN"))
	stopped, err := alice.CallTool(context.Background(), callToolReq("stop_yolo_detector", map[string]any{"robot_id": "alpha", "name": "people"}))
	if err != nil || stopped.IsError {
		t.Fatalf("stop: %+v, %v", stopped, err)
	}
	body, _ = json.Marshal(stopped.StructuredContent)
	if !strings.Contains(string(body), `"status":"stop_requested"`) || !strings.Contains(string(body), `"runtime_stop_verified":false`) || client.deployed.Inference.IsEnabled() {
		t.Fatal("stop request claimed runtime completion", string(body))
	}
	client.existing = nil
	client.deployed = nil
	client.sources = append(client.sources, &agentpbv2.DataSource{Id: "v4l2:/dev/video2", Kind: "camera", Healthy: true})
	result, err = deployGatewayYOLO(context.Background(), &GatewayRobot{ID: "alpha"}, s, callToolReq("deploy_yolo_detector", map[string]any{"name": "people", "model_ref": "acme/yolo"}), resolve)
	if err != nil || !result.IsError || client.deployed != nil {
		t.Fatal("ambiguous camera activated")
	}
	client.existing = &agentpbv2.DataCampaign{Name: "chatgpt-yolo-people", PlanJson: []byte(`{"name":"chatgpt-yolo-people"}`)}
	result, err = deployGatewayYOLO(context.Background(), &GatewayRobot{ID: "alpha"}, s, callToolReq("deploy_yolo_detector", map[string]any{"name": "people", "source_id": "v4l2:/dev/video0"}), resolve)
	if err != nil || !result.IsError || client.deployed != nil {
		t.Fatal("overwrote unrelated campaign")
	}
}

func TestGatewayYOLOAuthorization(t *testing.T) {
	connects := 0
	g, err := NewRobotGateway(gatewayTestConfig(), func(context.Context, string) (*grpcclient.AgentConnection, error) {
		connects++
		return nil, fmt.Errorf("unused")
	})
	if err != nil {
		t.Fatal(err)
	}
	server := gatewayTestHTTP(t, g)
	bob := gatewayHTTPClient(t, server.URL, gatewayTestEnv("BOB_TOKEN"))
	tools, err := bob.ListTools(context.Background(), mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if strings.Contains(tool.Name, "yolo_detector") {
			t.Fatal("YOLO tool exposed without inference scope", tool.Name)
		}
	}
	for _, tool := range []string{"deploy_yolo_detector", "inspect_yolo_detector", "stop_yolo_detector"} {
		result, err := bob.CallTool(context.Background(), callToolReq(tool, map[string]any{"robot_id": "alpha", "name": "people", "model_ref": "acme/yolo"}))
		if err == nil && !result.IsError {
			t.Fatal("cross-account YOLO operation allowed")
		}
	}
	if connects != 0 {
		t.Fatal("unauthorized request reached device")
	}
}
