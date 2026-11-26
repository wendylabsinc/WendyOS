package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

type chatModelTestClient struct{ pb.WendyContainerServiceClient }

func (c chatModelTestClient) ListContainers(context.Context, *pb.ListContainersRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.ListContainersResponse], error) {
	return &runtimeList{app: &pb.AppContainer{AppName: "sh.wendy.models.ollama.11437", HttpPort: 11437, RunningState: pb.AppRunningState_RUNNING}}, nil
}

func installChatModelTestConnection(t *testing.T, conn *grpcclient.AgentConnection) {
	t.Helper()
	previous := chatModelConnect
	t.Cleanup(func() { chatModelConnect = previous })
	chatModelConnect = func(ctx context.Context, device string) (*grpcclient.AgentConnection, error) {
		if device != "selected-local-device" {
			t.Errorf("inference connected to unexpected device %q", device)
		}
		return conn, nil
	}
}

func poisonChatModelEnvironment(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"WENDY_CHAT_PROVIDER": "openai", "WENDY_CHAT_MODEL": "hosted-model",
		"WENDY_CHAT_BASE_URL": "https://hosted.invalid/v1", "OPENAI_BASE_URL": "https://other.invalid/v1",
		"WENDY_CHAT_API_KEY": "wendy-test-secret", "OPENAI_API_KEY": "openai-test-secret", "ANTHROPIC_API_KEY": "anthropic-test-secret",
		"WENDY_CHAT_MAX_TOKENS": "99999", "HTTP_PROXY": "http://proxy.invalid", "HTTPS_PROXY": "http://proxy.invalid",
	} {
		t.Setenv(key, value)
	}
}

func TestChatModelDeviceUsesOnlySelectedAuthenticatedRuntime(t *testing.T) {
	poisonChatModelEnvironment(t)
	var requests atomic.Int32
	client := newFoxgloveExecTestClient(t, func(stream pb.WendyContainerService_ExecContainerServer) error {
		req, err := receiveModelHTTPRequest(stream)
		if err != nil {
			return err
		}
		requests.Add(1)
		if req.Header.Get("Authorization") != "" || req.Header.Get("X-Api-Key") != "" {
			return errors.New("hosted credential leaked to local runtime")
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return err
		}
		for _, forbidden := range []string{"hosted-model", "test-secret", "hosted.invalid"} {
			if strings.Contains(string(body), forbidden) {
				return errors.New("hosted configuration leaked to local request")
			}
		}
		var response string
		switch req.URL.Path {
		case "/v1/models":
			response = `{"data":[{"id":"local-tools"}]}`
		case "/api/version":
			response = `{"version":"test"}`
		case "/api/show":
			response = `{"capabilities":["completion","tools"]}`
		case "/api/chat":
			var payload struct {
				Model   string         `json:"model"`
				Options map[string]int `json:"options"`
				Tools   []any          `json:"tools"`
			}
			if err := json.Unmarshal(body, &payload); err != nil || payload.Model != "local-tools" || payload.Options["num_predict"] != 512 || len(payload.Tools) != 1 {
				return fmt.Errorf("local model/token/tool selection changed: %s", body)
			}
			response = `{"message":{"tool_calls":[{"function":{"name":"robot_status","arguments":{}}}]},"done":true}` + "\n"
		default:
			return fmt.Errorf("unexpected endpoint: %s", req.URL.Path)
		}
		wire := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(response), response)
		return stream.Send(&pb.ExecContainerResponse{ResponseType: &pb.ExecContainerResponse_StdoutData{StdoutData: []byte(wire)}})
	})
	installChatModelTestConnection(t, &grpcclient.AgentConnection{IsMTLS: true, ContainerService: chatModelTestClient{client}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	local, factory, close, err := connectChatModel(ctx, "selected-local-device", 11437, chat.Config{Model: "local-tools", MaxTokens: 512})
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	if local.Provider != "local" || local.Model != "local-tools" || local.APIKey != "" || local.BaseURL != "http://wendy-device/v1" {
		t.Fatal("session is not pinned to its device model")
	}
	provider, err := factory(local)
	if err != nil {
		t.Fatal(err)
	}
	message, err := provider.Stream(ctx, []chat.Message{{Role: "user", Content: "Check robot status"}}, []chat.Tool{{Name: "robot_status", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}, nil)
	if err != nil || len(message.ToolCalls) != 1 || message.ToolCalls[0].Name != "robot_status" || requests.Load() != 4 {
		t.Fatalf("local tool roundtrip failed: %+v, %v (%d requests)", message, err, requests.Load())
	}
	for _, mutate := range []func(*chat.Config){
		func(c *chat.Config) { c.Model = "other" }, func(c *chat.Config) { c.Provider = "openai" },
		func(c *chat.Config) { c.BaseURL = "https://hosted.invalid/v1" }, func(c *chat.Config) { c.APIKey = "secret" },
	} {
		changed := local
		mutate(&changed)
		if _, err := factory(changed); err == nil {
			t.Fatal("factory accepted alternate inference configuration")
		}
	}
}

func TestChatModelDeviceRefusesUnauthenticatedAndForeignRequests(t *testing.T) {
	for _, conn := range []*grpcclient.AgentConnection{nil, {Host: "192.0.2.1"}} {
		req, _ := http.NewRequest(http.MethodGet, "http://wendy-device/v1/models", nil)
		if _, err := (chatModelTransport{conn: conn, port: 11437}).RoundTrip(req); err == nil {
			t.Fatalf("accepted unauthenticated transport: %v", err)
		}
	}
	for _, url := range []string{"https://hosted.invalid/v1/models", "http://127.0.0.1/v1/models", "https://wendy-device/v1/models"} {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if _, err := (chatModelTransport{conn: &grpcclient.AgentConnection{IsMTLS: true}, port: 11437}).RoundTrip(req); err == nil {
			t.Fatal("foreign inference URL accepted")
		}
	}
}

func TestChatModelDeviceRequiresExplicitModelDespiteEnvironment(t *testing.T) {
	poisonChatModelEnvironment(t)
	previous := chatModelConnect
	t.Cleanup(func() { chatModelConnect = previous })
	chatModelConnect = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		t.Fatal("connected before validating explicit model/port")
		return nil, nil
	}
	for _, cfg := range []chat.Config{{}, {Model: "local", MaxTokens: -1}} {
		if _, _, _, err := connectChatModel(context.Background(), "selected-local-device", 11437, cfg); err == nil {
			t.Fatal("invalid explicit config accepted")
		}
	}
	for _, port := range []uint32{0, 65536} {
		if _, _, _, err := connectChatModel(context.Background(), "selected-local-device", port, chat.Config{Model: "local"}); err == nil {
			t.Fatal("invalid port accepted")
		}
	}
}

func TestChatModelHeadlessDoesNotMergeHostedEnvironment(t *testing.T) {
	poisonChatModelEnvironment(t)
	want := chat.Config{Provider: "local", Model: "local", BaseURL: "http://wendy-device/v1", MaxTokens: 512}
	stop := errors.New("stop before launching MCP")
	called := false
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runChatHeadless(cmd, chatHeadlessOptions{Config: want, Profile: chat.Profile{Name: "general"}, ProviderFactory: func(got chat.Config) (chat.Provider, error) {
		called = true
		if got != want {
			t.Error("headless chat changed pinned config using saved/environment settings")
		}
		return nil, stop
	}})
	if !called || !errors.Is(err, stop) {
		t.Fatalf("factory not used before starting tools: %v", err)
	}
}
