package commands

import (
	"bytes"
	"encoding/json"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
)

func readChatGPTJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func readChatGPTPolicy(t *testing.T, home string) wendymcp.RobotGatewayConfig {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".wendy", "chatgpt", "gateway.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := wendymcp.DecodeRobotGatewayConfig(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestChatGPTSetupStartsWithoutCloudDeviceOrDeveloperAccess(t *testing.T) {
	home := t.TempDir()
	binary := filepath.Join(home, "bin with spaces", "wendy")
	if _, err := setupChatGPT(home, binary, chatGPTSetupOptions{connection: "local"}); err != nil {
		t.Fatal(err)
	}
	cfg := readChatGPTPolicy(t, home)
	if len(cfg.Robots) != 0 || len(cfg.CloudSources) != 0 || cfg.AllowSimulators || cfg.AllowHostOperations || len(cfg.Workspaces) != 0 {
		t.Fatal("first-run policy granted operational access", cfg)
	}
	if !reflect.DeepEqual(cfg.Grants[0].Scopes, []string{wendymcp.RobotReadScope, wendymcp.RobotSettingsScope}) {
		t.Fatal("unexpected initial permissions", cfg.Grants)
	}
	root := filepath.Join(home, ".codex", "plugins", "sources", "wendy-robots")
	servers := readChatGPTJSON(t, filepath.Join(root, "mcp.json"))["mcpServers"].(map[string]any)
	if len(servers) != 1 {
		t.Fatal("developer tools must be opt-in", servers)
	}
	gateway := servers["wendy-robots"].(map[string]any)
	if gateway["command"] != binary || !reflect.DeepEqual(gateway["args"], []any{"mcp", "gateway", "--config", filepath.Join(home, ".wendy", "chatgpt", "gateway.json")}) {
		t.Fatal("wrong gateway routing", gateway)
	}
	manifest := readChatGPTJSON(t, filepath.Join(root, "plugin.json"))
	onboarding := manifest["extensions"].(map[string]any)["com.openai"].(map[string]any)["onboardingSkill"].(string)
	if _, err := os.Stat(filepath.Join(root, onboarding)); err != nil {
		t.Fatal("onboarding is unavailable without an MCP connection", err)
	}
	presentation := manifest["extensions"].(map[string]any)["com.openai"].(map[string]any)["interface"].(map[string]any)
	for _, field := range []string{"logo", "composerIcon"} {
		path, ok := presentation[field].(string)
		if !ok || path == "" {
			t.Fatalf("missing %s asset reference", field)
		}
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("%s asset was not exported: %v", field, err)
		}
		image, err := png.DecodeConfig(bytes.NewReader(raw))
		if err != nil || image.Width != image.Height || image.Width < 256 || len(raw) > 5*1024*1024 {
			t.Fatalf("invalid %s PNG: dimensions=%dx%d bytes=%d error=%v", field, image.Width, image.Height, len(raw), err)
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(filepath.Join(home, ".wendy", "chatgpt", "gateway.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("policy is not private", info, err)
	}
}

func TestChatGPTSetupMixedTargetsAndPermissionUpgradePreservesPolicy(t *testing.T) {
	home := t.TempDir()
	binary := filepath.Join(home, "wendy")
	local, cloud := "workshop.local:50052", "cloud://cloud.example:443/org/1/asset/2"
	tenantDevice := "cloud://" + strings.Repeat("a", 60) + ".example:443/tenant/00000000-0000-4000-8000-000000000001/asset/00000000-0000-4000-8000-000000000002"
	project := filepath.Join(home, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	o := chatGPTSetupOptions{connection: "local", devices: []string{local, cloud, tenantDevice}, workspaces: []string{project}, simulators: true, developer: true}
	if _, err := setupChatGPT(home, binary, o); err != nil {
		t.Fatal(err)
	}
	cfg := readChatGPTPolicy(t, home)
	if len(cfg.Robots) != 3 || cfg.Robots[0].Device != local || cfg.Robots[1].Device != cloud || cfg.Robots[2].Device != tenantDevice {
		t.Fatal("mixed local and Cloud targets lost", cfg.Robots)
	}
	for _, robot := range cfg.Robots {
		if robot.AllowAllApps || robot.AllowCamera || len(robot.Apps) != 0 {
			t.Fatal("simulator permissions widened physical target permissions", robot)
		}
	}
	if !cfg.AllowSimulators || !cfg.AllowSimulatorDeviceAccess || !cfg.Workspaces[0].AllowSimulators || !slices.Contains(cfg.Grants[0].Scopes, wendymcp.RobotProjectWriteScope) {
		t.Fatal("requested permissions not applied", cfg)
	}
	if _, err := setupChatGPT(home, binary, o); err != nil {
		t.Fatal(err)
	}
	again := readChatGPTPolicy(t, home)
	if !reflect.DeepEqual(cfg, again) {
		t.Fatal("repeated setup changed target identities or grants", cfg, again)
	}
	if _, err := setupChatGPT(home, binary, chatGPTSetupOptions{connection: "local"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, readChatGPTPolicy(t, home)) {
		t.Fatal("default rerun lost existing permissions")
	}
	servers := readChatGPTJSON(t, filepath.Join(home, ".codex", "plugins", "sources", "wendy-robots", "mcp.json"))["mcpServers"].(map[string]any)
	if len(servers) != 2 {
		t.Fatal("default rerun disabled an existing developer opt-in")
	}
	if _, err := setupChatGPT(home, binary, chatGPTSetupOptions{connection: "local", developerChanged: true}); err != nil {
		t.Fatal(err)
	}
	servers = readChatGPTJSON(t, filepath.Join(home, ".codex", "plugins", "sources", "wendy-robots", "mcp.json"))["mcpServers"].(map[string]any)
	if len(servers) != 1 {
		t.Fatal("explicit developer disable was ignored")
	}
}

func TestChatGPTSetupBothKeepsHostedIndependentAndMarketplaceEntries(t *testing.T) {
	home := t.TempDir()
	marketplace := filepath.Join(home, ".agents", "plugins", "marketplace.json")
	original := map[string]any{"name": "my-tools", "interface": map[string]any{"displayName": "My tools"}, "plugins": []any{map[string]any{"name": "other", "source": "./other"}}}
	if err := writeChatGPTJSON(marketplace, original); err != nil {
		t.Fatal(err)
	}
	o := chatGPTSetupOptions{connection: "both", appID: "asdk_app_existing", developer: true}
	if _, err := setupChatGPT(home, filepath.Join(home, "wendy"), o); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".codex", "plugins", "sources")
	hosted := filepath.Join(root, "wendy-robots-cloud")
	apps := readChatGPTJSON(t, filepath.Join(hosted, ".app.json"))["apps"].(map[string]any)
	if apps["wendy-robots"].(map[string]any)["id"] != o.appID {
		t.Fatal("hosted app binding changed", apps)
	}
	if _, err := os.Stat(filepath.Join(hosted, "mcp.json")); !os.IsNotExist(err) {
		t.Fatal("hosted package depends on a local MCP process", err)
	}
	servers := readChatGPTJSON(t, filepath.Join(root, "wendy-robots", "mcp.json"))["mcpServers"].(map[string]any)
	if len(servers) != 2 {
		t.Fatal("developer opt-in did not launch both local servers", servers)
	}
	got := readChatGPTJSON(t, marketplace)
	if got["name"] != original["name"] || !reflect.DeepEqual(got["interface"], original["interface"]) || len(got["plugins"].([]any)) != 3 {
		t.Fatal("existing marketplace lost", got)
	}
	for _, entry := range got["plugins"].([]any)[1:] {
		source := entry.(map[string]any)["source"].(map[string]any)["path"].(string)
		if _, err := os.Stat(filepath.Join(home, strings.TrimPrefix(source, "./"), "plugin.json")); err != nil {
			t.Fatal("marketplace source is not relative to home root", err)
		}
	}
	if _, err := setupChatGPT(home, filepath.Join(home, "wendy"), o); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, readChatGPTJSON(t, marketplace)) {
		t.Fatal("repeated setup duplicated marketplace entries")
	}
}

func TestChatGPTHostedSetupDoesNotCreateLocalPolicy(t *testing.T) {
	home := t.TempDir()
	if _, err := setupChatGPT(home, filepath.Join(home, "unused-wendy"), chatGPTSetupOptions{connection: "hosted", appID: "asdk_app_existing"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".wendy")); !os.IsNotExist(err) {
		t.Fatal("hosted setup created local permissions", err)
	}
}

func TestChatGPTSetupRejectsInvalidInputsBeforeWriting(t *testing.T) {
	for _, options := range []chatGPTSetupOptions{
		{connection: "both"}, {connection: "hosted", appID: "https://example.com/mcp"},
		{connection: "hosted", appID: "asdk_app_existing", simulators: true},
		{connection: "local", devices: []string{"cloud://broken"}},
	} {
		home := t.TempDir()
		if _, err := setupChatGPT(home, filepath.Join(home, "wendy"), options); err == nil {
			t.Fatal("invalid options accepted", options)
		}
		entries, err := os.ReadDir(home)
		if err != nil || len(entries) != 0 {
			t.Fatal("invalid setup changed the user's files", entries, err)
		}
	}
}
