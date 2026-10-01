package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

func TestGatewayWorkspaceResourceCacheIdentity(t *testing.T) {
	g, err := NewRobotGateway(gatewayTestConfig(), func(context.Context, string) (*grpcclient.AgentConnection, error) {
		t.Error("reading workspace metadata must not connect to a device")
		return nil, fmt.Errorf("unexpected device connection")
	})
	if err != nil {
		t.Fatal(err)
	}
	h := gatewayTestHTTP(t, g)
	c := gatewayHTTPClient(t, h.URL, gatewayTestEnv("ALICE_TOKEN"))
	catalog, err := c.ListTools(context.Background(), mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	entrypoints := map[string]string{}
	for _, tool := range catalog.Tools {
		if tool.Name != "open_robot" && tool.Name != "open_devices" {
			continue
		}
		var wire struct {
			Meta struct {
				UI struct {
					ResourceURI string `json:"resourceUri"`
				} `json:"ui"`
			} `json:"_meta"`
		}
		b, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &wire); err != nil {
			t.Fatal(err)
		}
		entrypoints[tool.Name] = wire.Meta.UI.ResourceURI
	}
	if len(entrypoints) != 2 {
		t.Fatalf("missing workspace entrypoint: %v", entrypoints)
	}
	read := func(uri string) mcpgo.TextResourceContents {
		t.Helper()
		var req mcpgo.ReadResourceRequest
		req.Params.URI = uri
		result, err := c.ReadResource(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Contents) != 1 {
			t.Fatalf("%s returned %d resources", uri, len(result.Contents))
		}
		content, ok := result.Contents[0].(mcpgo.TextResourceContents)
		if !ok || content.URI != uri || content.MIMEType != "text/html;profile=mcp-app" {
			t.Fatalf("%s returned a mismatched resource identity or type", uri)
		}
		return content
	}
	current := read(entrypoints["open_devices"])
	expectedURI := fmt.Sprintf("ui://wendy/device-workspace-%x.html", sha256.Sum256([]byte(current.Text)))
	if current.Text != robotPanelHTML || current.URI != expectedURI || entrypoints["open_robot"] != expectedURI {
		t.Fatal("entrypoints and bundle do not share the bundle's content-derived URI")
	}
	for _, uri := range []string{"ui://wendy/robot-v1.html", "ui://wendy/device-workspace-v2.html", "ui://wendy/device-workspace-v3.html"} {
		t.Run(uri, func(t *testing.T) {
			legacy := read(uri)
			if legacy.Text == current.Text || len(legacy.Text) > 2048 {
				t.Fatal("legacy URI must return only the small upgrade view")
			}
			if !strings.Contains(legacy.Text, "Manage app → Refresh tools") || !strings.Contains(legacy.Text, "new conversation") {
				t.Fatal("legacy view does not explain how to load the updated app")
			}
			for _, forbidden := range []string{"tools/call", "tools/list", "resources/read", "callServerTool", "openLink", "fetch(", "<iframe", "<form"} {
				if strings.Contains(strings.ToLower(legacy.Text), strings.ToLower(forbidden)) {
					t.Fatalf("legacy view can invoke a newer app contract: %s", forbidden)
				}
			}
			methods := map[string]bool{}
			for _, match := range regexp.MustCompile(`method:\s*"([^"]+)"`).FindAllStringSubmatch(legacy.Text, -1) {
				methods[match[1]] = true
			}
			if len(methods) != 2 || !methods["ui/initialize"] || !methods["ui/notifications/initialized"] {
				t.Fatalf("upgrade view must send only the MCP Apps lifecycle handshake: %v", methods)
			}
		})
	}
}

func TestGatewayDataHelpersAreAppCallableWithoutRenderingViews(t *testing.T) {
	g, err := NewRobotGateway(gatewayTestConfig(), func(context.Context, string) (*grpcclient.AgentConnection, error) {
		t.Error("listing helper metadata must not connect to a device")
		return nil, fmt.Errorf("unexpected device connection")
	})
	if err != nil {
		t.Fatal(err)
	}
	h := gatewayTestHTTP(t, g)
	c := gatewayHTTPClient(t, h.URL, gatewayTestEnv("ALICE_TOKEN"))
	catalog, err := c.ListTools(context.Background(), mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]bool{"list_robots": false, "inspect_robot": false, "read_device_settings": false, "read_device_logs": false, "read_device_metrics": false, "start_camera_preview": false}
	for _, tool := range catalog.Tools {
		if _, ok := wanted[tool.Name]; !ok {
			continue
		}
		wanted[tool.Name] = true
		var wire struct {
			Meta struct {
				UI struct {
					ResourceURI string   `json:"resourceUri"`
					Visibility  []string `json:"visibility"`
				} `json:"ui"`
				OutputTemplate string `json:"openai/outputTemplate"`
			} `json:"_meta"`
		}
		b, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &wire); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(wire.Meta.UI.Visibility, "app") || wire.Meta.UI.ResourceURI != "" || wire.Meta.OutputTemplate != "" {
			t.Fatalf("%s must be app-callable without attaching a render view", tool.Name)
		}
	}
	for name, found := range wanted {
		if !found {
			t.Errorf("helper %s missing from authorized catalog", name)
		}
	}
}
