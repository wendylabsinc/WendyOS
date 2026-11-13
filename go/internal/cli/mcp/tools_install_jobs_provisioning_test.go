package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

const mcpTestPassword = "hunter2-mcp-secret"

// jobStoreBackend starts real jobs in a temporary store, so results are the
// same JSON a client receives.
func jobStoreBackend(t *testing.T, started *[]onboarding.StartOptions) onboarding.Backend {
	t.Helper()
	store := onboarding.JobStore{Root: t.TempDir()}
	hooks := onboarding.JobHooks{Plan: func(_ context.Context, o onboarding.Options) (*onboarding.Plan, error) {
		return &onboarding.Plan{DeviceType: o.DeviceType, Method: "removable-media", Version: "1.2.3", ArtifactURL: "https://example.invalid/image.zst", ArtifactSHA256: strings.Repeat("a", 64), Target: &onboarding.Drive{ID: o.Drive}}, nil
	}}
	return onboarding.Backend{Start: func(ctx context.Context, o onboarding.StartOptions) (*onboarding.Job, error) {
		*started = append(*started, o)
		return store.Start(ctx, o, hooks)
	}}
}

func resultJSON(t *testing.T, r *mcpgo.CallToolResult) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallStartPassesFirstBootInputWithoutReturningPasswords(t *testing.T) {
	var started []onboarding.StartOptions
	s := New(&config.Config{}, nil)
	s.SetInstallationBackend(jobStoreBackend(t, &started))
	r, err := s.handleInstallStart(context.Background(), callToolReq("os_install_start", map[string]any{
		"device_type": "raspberry-pi-5", "drive": "/dev/test", "device_name": "nano-lab",
		"wifi": []any{map[string]any{"ssid": "Home", "password": mcpTestPassword, "priority": float64(5), "hidden": true, "security": "WPA2"}, map[string]any{"ssid": "Cafe"}},
	}))
	if err != nil || r.IsError {
		t.Fatalf("start failed: %v %s", err, resultJSON(t, r))
	}
	if text := resultJSON(t, r); strings.Contains(text, mcpTestPassword) {
		t.Fatalf("result leaks the password: %s", text)
	}
	p := started[0].Provisioning
	if p == nil || p.DeviceName != "nano-lab" || len(p.WiFi) != 2 {
		t.Fatalf("provisioning %+v", p)
	}
	if n := p.WiFi[0]; n.SSID != "Home" || n.Password != mcpTestPassword || n.Priority != 5 || !n.Hidden || n.Security != "WPA2" {
		t.Fatalf("network %v", n)
	}
	content, _ := r.StructuredContent.(*onboarding.Job)
	if content == nil {
		var job onboarding.Job
		if err := json.Unmarshal([]byte(toolResultText(t, r)), &job); err != nil {
			t.Fatal(err)
		}
		content = &job
	}
	if content.Provisioning == nil || content.Provisioning.DeviceName != "nano-lab" || strings.Join(content.Provisioning.WiFi, ",") != "Home,Cafe" {
		t.Fatalf("job summary %+v", content.Provisioning)
	}
}

func TestInstallStartWithoutFirstBootInputStatesTheDefault(t *testing.T) {
	var started []onboarding.StartOptions
	s := New(&config.Config{}, nil)
	s.SetInstallationBackend(jobStoreBackend(t, &started))
	r, err := s.handleInstallStart(context.Background(), callToolReq("os_install_start", map[string]any{"device_type": "raspberry-pi-5", "drive": "/dev/test"}))
	if err != nil || r.IsError {
		t.Fatalf("start failed: %v %s", err, resultJSON(t, r))
	}
	text := resultJSON(t, r)
	for _, want := range []string{`\"auto_name\":true`, `\"no_wifi\":true`, "Ethernet"} {
		if !strings.Contains(text, want) {
			t.Fatalf("result lacks %s: %s", want, text)
		}
	}
}

func TestInstallStartRejectsMalformedFirstBootInput(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetInstallationBackend(onboarding.Backend{Start: func(context.Context, onboarding.StartOptions) (*onboarding.Job, error) {
		t.Fatal("malformed first-boot input reached the backend")
		return nil, nil
	}})
	for name, extra := range map[string]map[string]any{
		"wifi not array":   {"wifi": map[string]any{"ssid": "Home", "password": mcpTestPassword}},
		"network string":   {"wifi": []any{"ssid=Home,password=" + mcpTestPassword}},
		"ssid number":      {"wifi": []any{map[string]any{"ssid": 7, "password": mcpTestPassword}}},
		"password number":  {"wifi": []any{map[string]any{"ssid": "Home", "password": 12345678}}},
		"fraction":         {"wifi": []any{map[string]any{"ssid": "Home", "password": mcpTestPassword, "priority": 1.5}}},
		"misspelled field": {"wifi": []any{map[string]any{"ssid": "Home", "pasword": mcpTestPassword}}},
		"no_wifi string":   {"no_wifi": "true"},
		"device_name list": {"device_name": []any{"lab"}},
	} {
		t.Run(name, func(t *testing.T) {
			args := map[string]any{"device_type": "raspberry-pi-5"}
			for k, v := range extra {
				args[k] = v
			}
			r, err := s.handleInstallStart(context.Background(), callToolReq("os_install_start", args))
			if err != nil || !r.IsError {
				t.Fatalf("accepted %v", name)
			}
			if text := resultJSON(t, r); strings.Contains(text, mcpTestPassword) || !strings.Contains(text, string(errCodeInvalidArgument)) {
				t.Fatalf("error %s", text)
			}
		})
	}
}

func TestInstallStartInvalidFirstBootValuesNeverEchoPasswords(t *testing.T) {
	var started []onboarding.StartOptions
	s := New(&config.Config{}, nil)
	s.SetInstallationBackend(jobStoreBackend(t, &started))
	for _, args := range []map[string]any{
		{"no_wifi": true, "wifi": []any{map[string]any{"ssid": "Home", "password": mcpTestPassword}}},
		{"wifi": []any{map[string]any{"ssid": "Home", "password": mcpTestPassword + "\n[device]"}}},
		{"wifi": []any{map[string]any{"ssid": "", "password": mcpTestPassword}}},
		{"device_name": "Not Valid", "wifi": []any{map[string]any{"ssid": "Home", "password": mcpTestPassword}}},
	} {
		args["device_type"], args["drive"] = "raspberry-pi-5", "/dev/test"
		r, err := s.handleInstallStart(context.Background(), callToolReq("os_install_start", args))
		if err != nil || !r.IsError {
			t.Fatalf("accepted %v", args)
		}
		if text := resultJSON(t, r); strings.Contains(text, mcpTestPassword) {
			t.Fatalf("error leaks the password: %s", text)
		}
	}
}

func TestInstallStartSchemaStaysCompatible(t *testing.T) {
	srv, err := New(&config.Config{}, nil).newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	start := srv.GetTool("os_install_start").Tool.InputSchema
	plan := srv.GetTool("os_install_plan").Tool.InputSchema
	for _, name := range []string{"device_name", "wifi", "no_wifi"} {
		if _, ok := start.Properties[name]; !ok {
			t.Errorf("os_install_start lacks %s", name)
		}
		if _, ok := plan.Properties[name]; ok {
			t.Errorf("read-only os_install_plan gained %s", name)
		}
		if schemaHasDefault(start.Properties[name]) {
			t.Errorf("%s has a default, which changes older calls", name)
		}
	}
	if strings.Join(start.Required, ",") != "device_type" {
		t.Fatalf("required = %v", start.Required)
	}
	// The published relay catalog still resolves against the current tools.
	if _, err := ResolveRelayManifest(deployedRelayManifest); err != nil {
		t.Fatal(err)
	}
}

func TestRelayRefusesWiFiPasswordsBeforeConfirmation(t *testing.T) {
	manifest, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var started []onboarding.StartOptions
	var confirmed []map[string]any
	var logged []string
	s := New(&config.Config{}, nil)
	s.relay = true
	s.SetInstallationBackend(jobStoreBackend(t, &started))
	s.setBindingPolicy(&BindingPolicy{Tools: []string{"os_install_start"}, RelayManifest: &manifest,
		Confirm: func(_ context.Context, _ string, args map[string]any) bool {
			confirmed = append(confirmed, args)
			return true
		},
		Log: func(tool, outcome string) { logged = append(logged, tool+" "+outcome) },
	})
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	reply := callProtocolTool(t, srv, "os_install_start", map[string]any{"device_type": "raspberry-pi-5", "drive": "/dev/test",
		"wifi": []any{map[string]any{"ssid": "Cafe"}, map[string]any{"ssid": "Home", "password": mcpTestPassword}}})
	encoded, _ := json.Marshal(reply)
	if !deniedByBinding(reply) || !strings.Contains(string(encoded), "wendy install jobs start") || !strings.Contains(string(encoded), "wendy mcp serve") {
		t.Fatalf("relay accepted a Wi-Fi password: %s", encoded)
	}
	if len(confirmed) != 0 || len(started) != 0 {
		t.Fatalf("password reached confirmation or the backend: %v %d", confirmed, len(started))
	}
	if strings.Contains(string(encoded)+strings.Join(logged, "\n"), mcpTestPassword) {
		t.Fatal("refusal or log leaks the password")
	}
	// The handler refuses on its own as well, whatever reaches it.
	r, _ := s.handleInstallStart(context.Background(), callToolReq("os_install_start", map[string]any{"device_type": "raspberry-pi-5", "wifi": []any{map[string]any{"ssid": "Home", "password": mcpTestPassword}}}))
	if !r.IsError || len(started) != 0 {
		t.Fatal("handler accepted a Wi-Fi password in relay mode")
	}
	// Names, open networks and no Wi-Fi carry no credential.
	for _, args := range []map[string]any{
		{"device_name": "nano-lab", "wifi": []any{map[string]any{"ssid": "Cafe", "password": ""}}},
		{"device_name": "nano-lab", "no_wifi": true},
	} {
		args["device_type"], args["drive"] = "raspberry-pi-5", "/dev/test"
		if reply := callProtocolTool(t, srv, "os_install_start", args); reply.IsError {
			encoded, _ := json.Marshal(reply)
			t.Fatalf("relay refused %v: %s", args, encoded)
		}
	}
	if len(started) != 2 || len(confirmed) != 2 {
		t.Fatalf("started %d confirmed %d", len(started), len(confirmed))
	}
}

func TestRelayCredentialArgumentScope(t *testing.T) {
	password := map[string]any{"wifi": []any{map[string]any{"ssid": "Home", "password": mcpTestPassword}}}
	if _, refused := relayCredentialArgument("os_install_plan", password); refused {
		t.Fatal("refused a tool that takes no Wi-Fi")
	}
	for _, args := range []map[string]any{
		password,
		{"wifi": []any{map[string]any{"ssid": "Home", "password": 12345678}}},
	} {
		if _, refused := relayCredentialArgument("os_install_start", args); !refused {
			t.Fatalf("missed a credential in %v", args)
		}
	}
	for _, args := range []map[string]any{
		{}, {"wifi": "Home"}, {"wifi": []any{"Home", nil, map[string]any{"ssid": "Cafe", "password": nil}}},
	} {
		if _, refused := relayCredentialArgument("os_install_start", args); refused {
			t.Fatalf("refused %v", args)
		}
	}
}

// credentialPaths lists nested schema properties that look like credentials.
func credentialPaths(prefix string, schema any) []string {
	m, _ := schema.(map[string]any)
	var paths []string
	if items, ok := m["items"]; ok {
		paths = append(paths, credentialPaths(prefix+"[]", items)...)
	}
	props, _ := m["properties"].(map[string]any)
	for name, child := range props {
		path := prefix + "." + name
		if strings.Contains(name, "password") || strings.Contains(name, "token") || strings.Contains(name, "secret") {
			paths = append(paths, path)
		}
		paths = append(paths, credentialPaths(path, child)...)
	}
	return paths
}

// A served tool may take a nested credential only when the relay refuses it.
func TestRelayManifestNestedCredentialsAreRefused(t *testing.T) {
	m, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, tool := range m.Tools {
		for name, schema := range tool.InputSchema.Properties {
			for _, path := range credentialPaths(name, schema) {
				found = append(found, tool.Name+" "+path)
			}
		}
	}
	if strings.Join(found, ",") != "os_install_start wifi[].password" {
		t.Fatalf("nested credentials %v; refuse each in relayCredentialArgument and list it here", found)
	}
	if _, refused := relayCredentialArgument("os_install_start", map[string]any{"wifi": []any{map[string]any{"ssid": "Home", "password": "x"}}}); !refused {
		t.Fatal("relay accepts the nested Wi-Fi password")
	}
}
