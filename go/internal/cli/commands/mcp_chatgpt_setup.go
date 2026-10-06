package commands

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/assets"
	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

type chatGPTSetupOptions struct {
	connection, appID, configPath string
	devices, workspaces           []string
	simulators, host, developer   bool
	developerChanged              bool
}

func newMCPChatGPTSetupCmd() *cobra.Command {
	o := chatGPTSetupOptions{connection: "local"}
	cmd := &cobra.Command{
		Use:   "chatgpt",
		Short: "Set up Wendy in ChatGPT, starting without a device or Cloud account",
		Long:  "Create a personal ChatGPT plugin marketplace. Local runs the scoped gateway on this computer. Hosted references an existing registered MCP app and needs no local CLI at runtime. Both creates separate local and hosted plugins. No device is required; simulator, project, host, and developer access are opt-in. This does not publish a plugin or deploy a hosted server.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.developerChanged = cmd.Flags().Changed("developer-tools")
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			paths, err := setupChatGPT(home, wendyBinaryPath(), o)
			if err != nil {
				return err
			}
			localMCP, _ := os.ReadFile(filepath.Join(home, ".codex", "plugins", "sources", "wendy-robots", "mcp.json"))
			developerEnabled := o.connection != "hosted" && strings.Contains(string(localMCP), `"wendy-developer"`)
			printChatGPTSetupResult(cmd.OutOrStdout(), paths, o.connection, developerEnabled)
			return nil
		},
	}
	cmd.Flags().StringVar(&o.connection, "connection", "local", "local, hosted, or both")
	cmd.Flags().StringVar(&o.appID, "app-id", "", "Existing registered MCP app ID for hosted access; not a URL or tunnel ID")
	cmd.Flags().StringVar(&o.configPath, "config", "", "Local gateway policy (default ~/.wendy/chatgpt/gateway.json)")
	cmd.Flags().StringArrayVar(&o.devices, "device", nil, "Authorize a device selector; repeat for local and Cloud devices")
	cmd.Flags().StringArrayVar(&o.workspaces, "workspace", nil, "Authorize project files and deployment in this directory; repeat for multiple projects")
	cmd.Flags().BoolVar(&o.simulators, "simulators", false, "Allow local simulator management, app control, and cameras")
	cmd.Flags().BoolVar(&o.host, "host-operations", false, "Allow local installation planning and jobs; disk erases still need exact-target authorization")
	cmd.Flags().BoolVar(&o.developer, "developer-tools", false, "Also launch the broader wendy mcp serve toolset")
	return cmd
}

func printChatGPTSetupResult(out io.Writer, paths []string, connection string, developerEnabled bool) {
	renderer := lipgloss.NewRenderer(out)
	muted := renderer.NewStyle().Foreground(tui.ColorDim)
	bold := renderer.NewStyle().Bold(true)
	for _, path := range paths {
		fmt.Fprintln(out, muted.Render("Configured "+path))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, bold.Render("Finish setup in ChatGPT"))
	fmt.Fprintln(out)
	fmt.Fprintln(out, bold.Render("1. Quit and reopen ChatGPT Desktop."))
	fmt.Fprintln(out, bold.Render("2. Open Plugins and select Personal."))
	install := "3. Open Wendy and select + to install it."
	if connection == "hosted" {
		install = "3. Open Wendy Cloud and select + to install it."
	} else if connection == "both" {
		install = "3. Install Wendy for local access or Wendy Cloud for hosted access."
	}
	fmt.Fprintln(out, bold.Render(install))
	fmt.Fprintln(out, bold.Render("4. Start a new conversation and ask:"))
	fmt.Fprintln(out, `   "Help me get started with my first device or a simulator."`)
	if connection != "hosted" {
		fmt.Fprintln(out)
		fmt.Fprintln(out, muted.Render("Local devices and simulators do not require a Wendy Cloud account."))
		fmt.Fprintln(out, muted.Render("No simulator was created and no device was connected."))
	}
	if developerEnabled {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Developer tools are enabled alongside the gateway. They have broader access and do not inherit the gateway's target or app restrictions.")
	}
}

var registeredMCPAppID = regexp.MustCompile(`^(plugin_asdk_app_|asdk_app_|connector_|templated_apps_)[A-Za-z0-9_-]+$`)

func setupChatGPT(home, binary string, o chatGPTSetupOptions) ([]string, error) {
	if o.connection != "local" && o.connection != "hosted" && o.connection != "both" {
		return nil, fmt.Errorf("connection must be local, hosted, or both")
	}
	if o.connection != "local" && !registeredMCPAppID.MatchString(o.appID) {
		return nil, fmt.Errorf("hosted access requires --app-id with the existing MCP app ID from ChatGPT Plugins; register your deployed gateway first")
	}
	if o.connection == "local" && o.appID != "" {
		return nil, fmt.Errorf("use --connection hosted or both with --app-id")
	}
	if o.connection == "hosted" && (o.configPath != "" || len(o.devices) > 0 || len(o.workspaces) > 0 || o.simulators || o.host || o.developer) {
		return nil, fmt.Errorf("local permission flags require --connection local or both")
	}
	if !filepath.IsAbs(home) || !filepath.IsAbs(binary) {
		return nil, fmt.Errorf("ChatGPT setup requires absolute home and CLI paths")
	}

	marketplacePath := filepath.Join(home, ".agents", "plugins", "marketplace.json")
	marketplace := map[string]any{}
	if raw, err := os.ReadFile(marketplacePath); err == nil {
		if err := json.Unmarshal(raw, &marketplace); err != nil {
			return nil, fmt.Errorf("read personal marketplace: %w", err)
		}
		if marketplace == nil {
			return nil, fmt.Errorf("personal marketplace must be an object")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if _, ok := marketplace["name"]; !ok {
		marketplace["name"] = "personal"
	}
	entries, ok := marketplace["plugins"].([]any)
	if !ok && marketplace["plugins"] != nil {
		return nil, fmt.Errorf("personal marketplace plugins must be an array")
	}
	paths := []string{}
	policyPath := o.configPath
	if o.connection != "hosted" {
		if policyPath == "" {
			policyPath = filepath.Join(home, ".wendy", "chatgpt", "gateway.json")
		}
		var err error
		policyPath, err = filepath.Abs(policyPath)
		if err != nil {
			return nil, err
		}
		if err := prepareChatGPTPolicy(policyPath, o); err != nil {
			return nil, err
		}
		paths = append(paths, policyPath)
	}
	for _, hosted := range []bool{false, true} {
		if (hosted && o.connection == "local") || (!hosted && o.connection == "hosted") {
			continue
		}
		name := "wendy-robots"
		if hosted {
			name = "wendy-robots-cloud"
		}
		pluginRoot := filepath.Join(home, ".codex", "plugins", "sources", name)
		if err := writeChatGPTPackage(pluginRoot, binary, policyPath, hosted, o); err != nil {
			return nil, err
		}
		entry := map[string]any{"name": name, "source": map[string]any{"source": "local", "path": "./" + filepath.ToSlash(filepath.Join(".codex", "plugins", "sources", name))}, "policy": map[string]any{"installation": "AVAILABLE", "authentication": "ON_USE"}, "category": "Productivity"}
		found := false
		for i, old := range entries {
			if e, ok := old.(map[string]any); ok && e["name"] == name {
				// Preserve workspace installation/authentication policy and extra keys.
				e["source"] = entry["source"]
				entries[i], found = e, true
				break
			}
		}
		if !found {
			entries = append(entries, entry)
		}
		paths = append(paths, pluginRoot)
	}
	marketplace["plugins"] = entries
	if err := writeChatGPTJSON(marketplacePath, marketplace); err != nil {
		return nil, err
	}
	return append(paths, marketplacePath), nil
}

func prepareChatGPTPolicy(path string, o chatGPTSetupOptions) error {
	cfg := wendymcp.RobotGatewayConfig{AllowEmptyInventory: true, LocalSubject: "local-owner", Robots: []wendymcp.GatewayRobot{}, Grants: []wendymcp.GatewayGrant{{Subject: "local-owner", Robots: []string{}, Scopes: []string{wendymcp.RobotReadScope, wendymcp.RobotSettingsScope}}}}
	if raw, err := os.ReadFile(path); err == nil {
		var err error
		cfg, err = wendymcp.DecodeRobotGatewayConfig(strings.NewReader(string(raw)))
		if err != nil {
			return fmt.Errorf("existing gateway policy: %w", err)
		}
		if cfg.LocalSubject == "" || cfg.HTTP != nil {
			return fmt.Errorf("use a local stdio policy for ChatGPT desktop")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	var grant *wendymcp.GatewayGrant
	for i := range cfg.Grants {
		if cfg.Grants[i].Subject == cfg.LocalSubject {
			grant = &cfg.Grants[i]
			break
		}
	}
	if grant == nil {
		return fmt.Errorf("local subject needs a grant")
	}
	addScope := func(scope string) {
		if !slices.Contains(grant.Scopes, scope) {
			grant.Scopes = append(grant.Scopes, scope)
		}
	}
	for _, device := range o.devices {
		device = strings.TrimSpace(device)
		if device == "" {
			return fmt.Errorf("device selector must not be empty")
		}
		if _, matched, err := parseCloudDeviceSelector(device); matched && err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(device))
		id := fmt.Sprintf("device-%x", digest[:8])
		for _, robot := range cfg.Robots {
			if robot.Device == device {
				id = robot.ID
				break
			}
		}
		if !slices.ContainsFunc(cfg.Robots, func(r wendymcp.GatewayRobot) bool { return r.ID == id }) {
			name := device
			if len(name) > 128 {
				name = "Device " + id
			}
			cfg.Robots = append(cfg.Robots, wendymcp.GatewayRobot{ID: id, Name: name, Device: device, Apps: []string{}, ListAllApps: true})
		}
		if !slices.Contains(grant.Robots, id) {
			grant.Robots = append(grant.Robots, id)
		}
		addScope(wendymcp.RobotReadScope)
	}
	if o.simulators {
		cfg.AllowSimulators, cfg.AllowSimulatorDeviceAccess = true, true
		for _, scope := range []string{wendymcp.RobotReadScope, wendymcp.RobotSimulatorScope, wendymcp.RobotControlScope, wendymcp.RobotCameraScope} {
			addScope(scope)
		}
	}
	if o.host {
		cfg.AllowHostOperations = true
		addScope(wendymcp.RobotHostScope)
	}
	for _, workspace := range o.workspaces {
		if strings.TrimSpace(workspace) == "" {
			return fmt.Errorf("workspace must not be empty")
		}
		path, err := filepath.Abs(workspace)
		if err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("workspace must be an existing directory: %s", workspace)
		}
		digest := sha256.Sum256([]byte(path))
		id := fmt.Sprintf("project-%x", digest[:8])
		index := slices.IndexFunc(cfg.Workspaces, func(w wendymcp.GatewayWorkspace) bool { return w.Path == path })
		if index < 0 {
			cfg.Workspaces = append(cfg.Workspaces, wendymcp.GatewayWorkspace{ID: id, Name: filepath.Base(path), Path: path, Robots: slices.Clone(grant.Robots), AllowSimulators: cfg.AllowSimulators})
		} else {
			w := &cfg.Workspaces[index]
			id = w.ID
			for _, robot := range grant.Robots {
				if !slices.Contains(w.Robots, robot) {
					w.Robots = append(w.Robots, robot)
				}
			}
			w.AllowSimulators = w.AllowSimulators || cfg.AllowSimulators
		}
		if !slices.Contains(grant.Workspaces, id) {
			grant.Workspaces = append(grant.Workspaces, id)
		}
		for _, scope := range []string{wendymcp.RobotProjectScope, wendymcp.RobotProjectWriteScope, wendymcp.RobotDeployScope} {
			addScope(scope)
		}
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if _, err := wendymcp.DecodeRobotGatewayConfig(strings.NewReader(string(encoded))); err != nil {
		return err
	}
	return writeChatGPTJSON(path, cfg)
}

func writeChatGPTPackage(root, binary, policy string, hosted bool, o chatGPTSetupOptions) error {
	raw, err := assets.FS.ReadFile("chatgpt-plugin/plugin.json")
	if err != nil {
		return err
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return err
	}
	extension := manifest["extensions"].(map[string]any)["com.openai"].(map[string]any)
	if hosted {
		manifest["name"] = "wendy-robots-cloud"
		presentation := extension["interface"].(map[string]any)
		presentation["displayName"] = "Wendy Cloud"
		presentation["shortDescription"] = "Your connected devices"
		presentation["longDescription"] = "Get started with your connected robots and small computers in ChatGPT. Browse authorized devices, inspect their apps, read telemetry, and use permitted camera and app controls through the hosted connection. No local CLI is required. For laptop simulators or project files, choose the separate local desktop connection."
		extension["apps"] = "./.app.json"
		manifest["description"] = "Work with your connected devices in ChatGPT through a registered Wendy gateway. No local CLI is required."
		if err := writeChatGPTJSON(filepath.Join(root, ".app.json"), map[string]any{"apps": map[string]any{"wendy-robots": map[string]any{"id": o.appID, "required": true}}}); err != nil {
			return err
		}
	} else {
		servers := map[string]any{}
		if raw, err := os.ReadFile(filepath.Join(root, "mcp.json")); err == nil {
			var existing struct {
				Servers map[string]any `json:"mcpServers"`
			}
			if err := json.Unmarshal(raw, &existing); err != nil {
				return fmt.Errorf("existing desktop MCP configuration: %w", err)
			}
			if existing.Servers != nil {
				servers = existing.Servers
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		gateway, ok := servers["wendy-robots"].(map[string]any)
		if !ok {
			gateway = map[string]any{}
		}
		gateway["type"], gateway["command"], gateway["args"] = "stdio", binary, []string{"mcp", "gateway", "--config", policy}
		servers["wendy-robots"] = gateway
		if o.developer {
			developer, ok := servers["wendy-developer"].(map[string]any)
			if !ok {
				developer = map[string]any{}
			}
			developer["type"], developer["command"] = "stdio", binary
			args, ok := developer["args"].([]any)
			if !ok || len(args) < 2 || args[0] != "mcp" || args[1] != "serve" {
				developer["args"] = []string{"mcp", "serve"}
			}
			servers["wendy-developer"] = developer
		} else if o.developerChanged {
			delete(servers, "wendy-developer")
		} else if developer, ok := servers["wendy-developer"].(map[string]any); ok {
			developer["command"] = binary
		}
		if err := writeChatGPTJSON(filepath.Join(root, "mcp.json"), map[string]any{"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json", "mcpServers": servers}); err != nil {
			return err
		}
	}
	if err := writeChatGPTJSON(filepath.Join(root, "plugin.json"), manifest); err != nil {
		return err
	}
	if err := fs.WalkDir(assets.FS, "chatgpt-plugin/assets", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, err := assets.FS.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path, "chatgpt-plugin/")))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return writeFileAtomic(target, raw, 0o600)
	}); err != nil {
		return err
	}
	return fs.WalkDir(assets.FS, "mcp-skills/wendy", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, err := assets.FS.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(root, "skills", "wendy", filepath.FromSlash(strings.TrimPrefix(path, "mcp-skills/wendy/")))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return writeFileAtomic(target, raw, 0o600)
	})
}

func writeChatGPTJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, append(raw, '\n'), 0o600)
}
