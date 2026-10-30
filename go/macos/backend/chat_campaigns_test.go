package backend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	legacy "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

const personArgs = `{"name":"door-people","camera":"camera:front","notify":true,"upload":false,"threshold":0.9,"cooldown_seconds":30,"before_seconds":10,"after_seconds":10}`

type campaignChatClient struct {
	pb.DataServiceClient
	healthy, exists bool
	deployed        []byte
	reads           int
}

func (s *campaignChatClient) Sources(context.Context, *pb.DataSourcesRequest, ...grpc.CallOption) (*pb.DataSourcesResponse, error) {
	s.reads++
	return &pb.DataSourcesResponse{Sources: []*pb.DataSource{{Id: "camera:front", Kind: "camera", Healthy: s.healthy}}}, nil
}
func (s *campaignChatClient) Campaigns(context.Context, *pb.DataCampaignsRequest, ...grpc.CallOption) (*pb.DataCampaignsResponse, error) {
	s.reads++
	r := &pb.DataCampaignsResponse{}
	if s.exists {
		r.Campaigns = []*pb.DataCampaign{{Name: "door-people"}}
	}
	return r, nil
}
func (s *campaignChatClient) CampaignDeploy(_ context.Context, req *pb.DataCampaignDeployRequest, _ ...grpc.CallOption) (*pb.DataCampaign, error) {
	s.deployed = append([]byte(nil), req.CampaignYaml...)
	return &pb.DataCampaign{}, nil
}

type campaignChatEnrollment struct {
	legacy.WendyProvisioningServiceClient
}

func (campaignChatEnrollment) IsProvisioned(context.Context, *legacy.IsProvisionedRequest, ...grpc.CallOption) (*legacy.IsProvisionedResponse, error) {
	return &legacy.IsProvisionedResponse{Response: &legacy.IsProvisionedResponse_NotProvisioned{NotProvisioned: &legacy.NotProvisionedResponse{}}}, nil
}
func campaignToolsForTest(client *campaignChatClient) *deviceCampaignTools {
	return &deviceCampaignTools{Executor: &testDesktopChatTools{}, conn: &grpcclient.AgentConnection{DataService: client, ProvisioningService: campaignChatEnrollment{}}}
}
func TestChatCampaignCreationRequiresApproval(t *testing.T) {
	for _, allow := range []bool{false, true} {
		client := &campaignChatClient{healthy: true}
		executor := &deviceCardTools{Executor: campaignToolsForTest(client), turn: &ChatTurn{}}
		engine := chat.NewEngine(&deviceToolTestProvider{call: chat.ToolCall{Name: "create_person_detection_campaign", Arguments: json.RawMessage(personArgs)}}, executor, deviceChatPrompt())
		approvals := 0
		if err := engine.Turn(context.Background(), "notify me when a person appears", nil, func(_ context.Context, call chat.ToolCall) (bool, error) { approvals++; return allow, nil }); err != nil {
			t.Fatal(err)
		}
		if approvals != 1 {
			t.Fatalf("approval calls=%d", approvals)
		}
		if !allow {
			if client.reads != 0 || len(client.deployed) != 0 {
				t.Fatal("denied campaign performed RPCs")
			}
			continue
		}
		plan, err := data.ParseCampaign(client.deployed)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Inference == nil || plan.Inference.Model != "facebook/detr-resnet-50" || len(plan.Inference.Revision) != 40 || len(plan.Inference.Labels) != 1 || plan.Inference.Labels[0] != "person" {
			t.Fatalf("wrong detector: %+v", plan.Inference)
		}
		if plan.Sources[0].Camera != "camera:front" || plan.Notify == nil || plan.Upload.When != "manual" || plan.Capture.Buffer != "10s" {
			t.Fatal("reviewed settings changed")
		}
		if !strings.Contains(engine.Messages()[len(engine.Messages())-1].Content, `"notification_delivery_verified":false`) {
			t.Fatal("missing delivery limitation")
		}
	}
}
func TestChatCampaignRejectsUnavailableDuplicateAndCancelled(t *testing.T) {
	for _, client := range []*campaignChatClient{{healthy: false}, {healthy: true, exists: true}} {
		if _, err := campaignToolsForTest(client).Execute(context.Background(), chat.ToolCall{Name: "create_person_detection_campaign", Arguments: json.RawMessage(personArgs)}); err == nil {
			t.Fatal("invalid campaign accepted")
		}
		if len(client.deployed) != 0 {
			t.Fatal("invalid campaign deployed")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &campaignChatClient{healthy: true}
	if _, err := campaignToolsForTest(client).Execute(ctx, chat.ToolCall{Name: "create_person_detection_campaign", Arguments: json.RawMessage(personArgs)}); err == nil || client.reads != 0 {
		t.Fatal("cancelled session performed work")
	}
}
func TestChatCampaignArgumentsAreCompleteAndBounded(t *testing.T) {
	for _, raw := range []string{`{}`, strings.Replace(personArgs, `"after_seconds":10`, `"after_seconds":0`, 1), strings.Replace(personArgs, `"threshold":0.9`, `"threshold":1.5`, 1), strings.Replace(personArgs, `"notify":true,`, "", 1), strings.Replace(personArgs, `"name":`, `"unknown":true,"name":`, 1), personArgs + `{}`} {
		if _, err := personCampaignInput([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid input %s", raw)
		}
	}
}
func TestDeviceChatLoadsCampaignWorkflowAtStartup(t *testing.T) {
	prompt := deviceChatPrompt()
	for _, text := range []string{"# Wendy device chat", "create_person_detection_campaign", "depth", "notification permission"} {
		if !strings.Contains(prompt, text) {
			t.Fatalf("missing startup guidance %s", text)
		}
	}
}

func TestChatNotificationGrantRequiresSeparateApproval(t *testing.T) {
	for _, allow := range []bool{false, true} {
		tools := campaignToolsForTest(&campaignChatClient{})
		writes := 0
		tools.enableNotifications = func(_ context.Context, conn *grpcclient.AgentConnection, name string) error {
			if conn != tools.conn || name != "door-people" {
				t.Fatal("permission targeted a different device or campaign")
			}
			writes++
			return nil
		}
		engine := chat.NewEngine(&deviceToolTestProvider{call: chat.ToolCall{Name: "enable_campaign_notifications", Arguments: json.RawMessage(`{"campaign":"door-people"}`)}}, tools, deviceChatPrompt())
		approvals := 0
		if err := engine.Turn(context.Background(), "enable notifications", nil, func(_ context.Context, call chat.ToolCall) (bool, error) {
			approvals++
			if writes != 0 || call.Name != "enable_campaign_notifications" {
				t.Fatal("Cloud write preceded review")
			}
			return allow, nil
		}); err != nil {
			t.Fatal(err)
		}
		if approvals != 1 || (allow && writes != 1) || (!allow && writes != 0) {
			t.Fatalf("allow=%v approvals=%d writes=%d", allow, approvals, writes)
		}
	}
}

func TestChatNotificationGrantRejectsMalformedOrCancelled(t *testing.T) {
	tools := campaignToolsForTest(&campaignChatClient{})
	tools.enableNotifications = func(context.Context, *grpcclient.AgentConnection, string) error {
		t.Fatal("unexpected Cloud write")
		return nil
	}
	for _, raw := range []string{`{}`, `{"campaign":""}`, `{"campaign":"people","organization":"elsewhere"}`, `{"campaign":"people"}{}`} {
		if _, err := tools.Execute(context.Background(), chat.ToolCall{Name: "enable_campaign_notifications", Arguments: json.RawMessage(raw)}); err == nil {
			t.Fatal("accepted invalid arguments")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tools.Execute(ctx, chat.ToolCall{Name: "enable_campaign_notifications", Arguments: json.RawMessage(`{"campaign":"people"}`)}); err == nil {
		t.Fatal("accepted cancelled call")
	}
}

func TestChatInvalidCampaignNameDoesNotReachApproval(t *testing.T) {
	tools := campaignToolsForTest(&campaignChatClient{healthy: true})
	engine := chat.NewEngine(&deviceToolTestProvider{call: chat.ToolCall{Name: "create_person_detection_campaign", Arguments: json.RawMessage(strings.Replace(personArgs, "door-people", "Person Detection", 1))}}, tools, deviceChatPrompt())
	if err := engine.Turn(context.Background(), "create person detection", nil, func(context.Context, chat.ToolCall) (bool, error) {
		t.Fatal("invalid name reached approval")
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCreatedCampaignPromptsForCloudPermissionAutomatically(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		notify, allow bool
		cancel        bool
		fail          bool
		want          string
	}{
		{"allow", true, true, false, false, "enabled"},
		{"deny", true, false, false, false, "denied"},
		{"cancel", true, true, true, false, "cancelled"},
		{"cloud failure", true, true, false, true, "failed"},
		{"no notifications", false, true, false, false, ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			client := &campaignChatClient{healthy: true}
			tools := campaignToolsForTest(client)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			prompts, writes := 0, 0
			tools.approveNotifications = func(_ context.Context, call chat.ToolCall) (bool, error) {
				prompts++
				if len(client.deployed) == 0 || writes != 0 || call.Name != "enable_campaign_notifications" || string(call.Arguments) != `{"campaign":"door-people"}` {
					t.Fatal("permission was not scoped to the newly created campaign")
				}
				if scenario.cancel {
					cancel()
				}
				return scenario.allow, nil
			}
			tools.enableNotifications = func(_ context.Context, conn *grpcclient.AgentConnection, name string) error {
				writes++
				if conn != tools.conn || name != "door-people" {
					t.Fatal("wrong Cloud grant target")
				}
				if scenario.fail {
					return errors.New("permission denied")
				}
				return nil
			}
			args := personArgs
			if !scenario.notify {
				args = strings.Replace(args, `"notify":true`, `"notify":false`, 1)
			}
			result, err := tools.Execute(ctx, chat.ToolCall{Name: "create_person_detection_campaign", Arguments: json.RawMessage(args)})
			if err != nil {
				t.Fatal(err)
			}
			if scenario.notify {
				if prompts != 1 || !strings.Contains(result, `"status":"`+scenario.want+`"`) {
					t.Fatalf("prompts=%d result=%s", prompts, result)
				}
			} else if prompts != 0 {
				t.Fatal("unrequested notification prompt")
			}
			wantWrites := 0
			if scenario.notify && scenario.allow && !scenario.cancel {
				wantWrites = 1
			}
			if writes != wantWrites {
				t.Fatalf("Cloud writes=%d want=%d", writes, wantWrites)
			}
		})
	}
}
