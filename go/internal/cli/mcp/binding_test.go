package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func bindingServer(t *testing.T, p *BindingPolicy) *server.MCPServer {
	t.Helper()
	s := New(&config.Config{}, nil)
	s.toolGroups = []string{"all"}
	s.setBindingPolicy(p)
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func callProtocolTool(t *testing.T, srv *server.MCPServer, name string, args ...map[string]any) mcpgo.CallToolResult {
	t.Helper()
	encodedArgs := []byte("{}")
	if len(args) > 0 {
		encodedArgs, _ = json.Marshal(args[0])
	}
	msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, encodedArgs)
	encoded, _ := json.Marshal(srv.HandleMessage(context.Background(), []byte(msg)))
	var reply struct {
		Result mcpgo.CallToolResult
		Error  json.RawMessage
	}
	if err := json.Unmarshal(encoded, &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Error) > 0 {
		t.Fatalf("tool call returned JSON-RPC error: %s", reply.Error)
	}
	return reply.Result
}

// mcp-go rejects filtered tools before invoking tool middleware. Verify the
// wire error as well as the tests' confirmation and visibility checks.
func assertProtocolToolHidden(t *testing.T, srv *server.MCPServer, name string) {
	t.Helper()
	msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, name)
	response := srv.HandleMessage(context.Background(), []byte(msg))
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Result json.RawMessage
		Error  *struct {
			Code    int
			Message string
		}
	}
	if err := json.Unmarshal(encoded, &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Result) != 0 || reply.Error == nil || reply.Error.Code != mcpgo.INVALID_PARAMS || !strings.Contains(reply.Error.Message, "tool '"+name+"' not found") {
		t.Fatalf("filtered tool %s was not rejected: %s", name, encoded)
	}
}

func deniedByBinding(r mcpgo.CallToolResult) bool {
	content, _ := r.StructuredContent.(map[string]any)
	code, _ := content["error_code"].(string)
	return r.IsError && code == string(errCodeDenied)
}

func TestBindingAllowListRefusesUnlistedTools(t *testing.T) {
	var asked []string
	p := &BindingPolicy{Confirm: func(_ context.Context, tool string, _ map[string]any) bool {
		asked = append(asked, tool)
		return true
	}}
	srv := bindingServer(t, p)

	assertProtocolToolHidden(t, srv, "device_disconnect")
	if len(asked) != 0 {
		t.Fatalf("a refused tool must not reach confirmation; asked %v", asked)
	}
	for _, tool := range protocolTools(t, srv) {
		if tool.Name == "device_disconnect" {
			t.Fatal("tools/list advertises a tool the binding does not allow")
		}
	}

	p.Tools = []string{"device_disconnect"}
	if r := callProtocolTool(t, srv, "device_disconnect"); deniedByBinding(r) {
		t.Fatalf("--allow-tool device_disconnect: still denied: %+v", r)
	}
	if !slices.Equal(asked, []string{"device_disconnect"}) {
		t.Fatalf("an allowed mutating tool must be confirmed; asked %v", asked)
	}
}

func TestBindingDefaultConfirmsReads(t *testing.T) {
	for _, decision := range []string{"no callback", "deny", "allow"} {
		t.Run(decision, func(t *testing.T) {
			var asked []string
			p := &BindingPolicy{}
			if decision != "no callback" {
				p.Confirm = func(_ context.Context, tool string, _ map[string]any) bool {
					asked = append(asked, tool)
					return decision == "allow"
				}
			}
			srv := bindingServer(t, p)
			if r := callProtocolTool(t, srv, "wendy_status"); deniedByBinding(r) != (decision != "allow") {
				t.Fatalf("read-only wendy_status with %s confirmation: %+v", decision, r)
			}
			if decision != "no callback" && !slices.Equal(asked, []string{"wendy_status"}) {
				t.Fatalf("default read must ask for confirmation; asked %v", asked)
			}
		})
	}
}

func TestBindingAllowReadConfirmsMutatingCallsNotReads(t *testing.T) {
	var asked []string
	srv := bindingServer(t, &BindingPolicy{AllowRead: true, Tools: []string{"device_disconnect"}, Confirm: func(_ context.Context, tool string, _ map[string]any) bool {
		asked = append(asked, tool)
		return false
	}})

	if r := callProtocolTool(t, srv, "wendy_status"); deniedByBinding(r) {
		t.Fatalf("read-only wendy_status was refused: %+v", r)
	}
	if len(asked) != 0 {
		t.Fatalf("a read must not ask for confirmation; asked %v", asked)
	}
	assertProtocolToolHidden(t, srv, "container_stop")
	if len(asked) != 0 {
		t.Fatalf("an unlisted mutation must not reach confirmation; asked %v", asked)
	}
	if r := callProtocolTool(t, srv, "device_disconnect"); !deniedByBinding(r) {
		t.Fatalf("unconfirmed device_disconnect: got %+v, want DENIED", r)
	}
	if !slices.Equal(asked, []string{"device_disconnect"}) {
		t.Fatalf("asked %v, want [device_disconnect]", asked)
	}

	// AAA §5.1 high-impact actions (deploy, container control, config push,
	// tunnel open, unenroll) and other state changes must never pass as
	// read-only, or a bound web client could run them unconfirmed without
	// --allow-all.
	for _, name := range []string{"run", "container_start", "container_stop", "container_delete", "container_exec",
		"cloud_tunnel", "cloud_enroll_device", "device_update_agent", "os_update", "os_install_start",
		"wifi_connect", "camera_set_control", "auth_login"} {
		if readOnlyTool(srv.GetTool(name).Tool) {
			t.Errorf("%s is read-only, so a bound web client could call it unconfirmed", name)
		}
	}
}

func TestBindingAllowReadNeedsNoConfirmationCallback(t *testing.T) {
	srv := bindingServer(t, &BindingPolicy{AllowRead: true, Tools: []string{"device_disconnect"}})
	if r := callProtocolTool(t, srv, "wendy_status"); r.IsError {
		t.Fatalf("--allow-read wendy_status failed: %+v", r)
	}
	if r := callProtocolTool(t, srv, "device_disconnect"); !deniedByBinding(r) {
		t.Fatalf("--allow-read must deny an allowed mutation without a confirmation callback: %+v", r)
	}
}

func TestBindingAllowAllSkipsToolConfirmation(t *testing.T) {
	for _, withConfirm := range []bool{false, true} {
		t.Run(fmt.Sprintf("confirm_callback_%t", withConfirm), func(t *testing.T) {
			p := &BindingPolicy{AllowAll: true}
			if withConfirm {
				p.Confirm = func(_ context.Context, tool string, _ map[string]any) bool {
					t.Errorf("--allow-all asked for confirmation of %s", tool)
					return false
				}
			}
			s := New(&config.Config{}, nil)
			s.setBindingPolicy(p)
			scanned := false
			s.SetLANDiscoverer(func(context.Context, time.Duration) ([]models.LANDevice, error) {
				scanned = true
				return nil, nil
			})
			srv, err := s.newProtocolServer()
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range []struct {
				name string
				args map[string]any
			}{
				{name: "wendy_status"},
				{name: "device_list"},
				{name: "device_list", args: map[string]any{"scan": true}},
				{name: "device_disconnect"},
			} {
				if result := callProtocolTool(t, srv, call.name, call.args); result.IsError {
					t.Errorf("--allow-all %s %v failed: %+v", call.name, call.args, result)
				}
			}
			if !scanned {
				t.Error("--allow-all device_list scan=true did not reach LAN discovery")
			}
		})
	}
}

func TestReadOnlyToolIgnoresProxiedAnnotations(t *testing.T) {
	proxied := mcpgo.NewTool("myapp_reset", readOnly()...)
	if readOnlyTool(proxied) {
		t.Fatal("a container tool's own read-only claim must not skip confirmation")
	}
}

func TestRealmAllowsMCP(t *testing.T) {
	token := func(claims string) *config.AuthConfig {
		return &config.AuthConfig{Name: "work", APIKey: "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".sig"}
	}
	if err := RealmAllowsMCP(token(`{"allow_mcp":false}`)); err == nil {
		t.Fatal("allow_mcp=false must refuse MCP")
	}
	for name, auth := range map[string]*config.AuthConfig{
		"claim true":    token(`{"allow_mcp":true}`),
		"claim absent":  token(`{"sub":"u"}`),
		"personal key":  {APIKey: "wendy_pat_abc"},
		"not logged in": nil,
	} {
		if err := RealmAllowsMCP(auth); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestBindingAllowReadStaysOffTheHost(t *testing.T) {
	var asked []string
	p := &BindingPolicy{AllowRead: true, Confirm: func(_ context.Context, tool string, _ map[string]any) bool {
		asked = append(asked, tool)
		return false
	}}
	srv := bindingServer(t, p)
	probes := []string{"os_install_verify", "project_validate", "os_list_drives", "os_install_plan", "os_install_status", "app_inspect", "device_list", "cloud_ping", "simulator_list"}
	for _, tool := range protocolTools(t, srv) {
		if slices.Contains(probes, tool.Name) {
			t.Errorf("the read-only preset advertises %s, which reaches the host", tool.Name)
		}
	}
	for _, name := range probes {
		assertProtocolToolHidden(t, srv, name)
	}
	if len(asked) != 0 {
		t.Fatalf("a refused tool must not reach confirmation; asked %v", asked)
	}

	p.Tools = probes
	for _, name := range probes {
		callProtocolTool(t, srv, name)
	}
	if !slices.Equal(asked, probes) {
		t.Fatalf("an allowed host tool must be confirmed like a state change; asked %v", asked)
	}
}

func TestBindingAllowReadPinsTheStartingSession(t *testing.T) {
	p := &BindingPolicy{
		AllowRead: true,
		Session:   &config.AuthConfig{CloudGRPC: "cloud.example:443"},
		Confirm: func(_ context.Context, tool string, _ map[string]any) bool {
			t.Errorf("a call naming another session must not reach confirmation: %s", tool)
			return true
		},
	}
	srv := bindingServer(t, p)
	r := callProtocolTool(t, srv, "cloud_discover", map[string]any{"cloud_grpc": "other.example:443"})
	if !deniedByBinding(r) || !strings.Contains(r.Content[0].(mcpgo.TextContent).Text, "another Wendy Cloud session") {
		t.Fatalf("--allow-read must refuse reads from another session: %+v", r)
	}
}

func TestBindingPinsTheStartingSession(t *testing.T) {
	const tenant, other = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	session := func(name, endpoint, tenant string, org int) config.AuthConfig {
		return config.AuthConfig{Name: name, CloudGRPC: endpoint, APIKey: "k", Certificates: []config.CertificateInfo{{
			PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/op", OrganizationID: org,
		}}}
	}
	pinned := session("work", "cloud.example:443", tenant, 7)
	cfg := &config.Config{CurrentContext: "home", Auth: []config.AuthConfig{
		pinned, session("home", "cloud.example:443", other, 8), session("lab", "lab.example:443", other, 9),
	}}
	var asked []string
	p := &BindingPolicy{AllowAll: true, Session: &pinned, Confirm: func(_ context.Context, tool string, _ map[string]any) bool {
		asked = append(asked, tool)
		return false
	}}
	s := New(cfg, nil)
	s.toolGroups = []string{"all"}
	s.setBindingPolicy(p)
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}

	for name, args := range map[string]map[string]any{
		"cloud_discover": {"cloud_grpc": "lab.example:443"},
		"device_connect": {"device": "cloud://cloud.example:443/tenant/" + other + "/asset/" + tenant},
		"run":            {"device": "CLOUD://cloud.example:443/org/8/asset/3"},
	} {
		if r := callProtocolTool(t, srv, name, args); !deniedByBinding(r) || !strings.Contains(r.Content[0].(mcpgo.TextContent).Text, "another Wendy Cloud session") {
			t.Errorf("%s %v: got %+v, want DENIED for another session", name, args, r)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("a call naming another session must not reach confirmation; asked %v", asked)
	}
	for _, args := range []map[string]any{
		{"cloud_grpc": "cloud.example:443"},
		{"device": "cloud://cloud.example:443/tenant/" + tenant + "/asset/" + other},
		{"device": "cloud://cloud.example:443/org/7/asset/3"},
	} {
		if key, other := p.otherSession(args); other {
			t.Errorf("%v: %s refused, but it names the pinned session", args, key)
		}
	}

	// The default context is "home"; a relay-fed call still resolves "work".
	auth, err := s.cloudAuthEntry("")
	if err != nil || auth.Name != "work" {
		t.Fatalf("cloudAuthEntry = %v, %v; want the pinned session", auth, err)
	}
	cfg.Auth[0] = session("work", "cloud.example:443", other, 8) // re-login to another org under the same name
	if _, err := s.cloudAuthEntry(""); err == nil {
		t.Fatal("the pinned session changed organization; cloud calls must stop")
	}
}

func TestCheckToolNames(t *testing.T) {
	if err := CheckToolNames([]string{"device_disconnect", "os_install_verify"}); err != nil {
		t.Fatal(err)
	}
	err := CheckToolNames([]string{"device_disconect"})
	if err == nil || !strings.Contains(err.Error(), `"device_disconect"`) || !strings.Contains(err.Error(), "device_disconnect") {
		t.Fatalf("a typo must fail and list the valid names; got %v", err)
	}
}
