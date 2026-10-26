package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// BindingPolicy is what one web client bound through the hosted MCP relay may
// do (Relay MCP spec, WDY-3660). The relay can forge any call, so the CLI is
// where the limits hold: a tool outside the allow-list is refused. Allowed
// calls require confirmation unless AllowAll or the read-only AllowRead
// exemption applies. The local stdio server has no policy.
type BindingPolicy struct {
	AllowAll  bool     // allow every tool without per-call confirmation
	AllowRead bool     // skip confirmation for built-in reads that stay off the host
	Tools     []string // further tools allowed by name
	// Session is the auth session the run started with. Cloud calls use it
	// alone; an argument naming another session or organization is refused.
	Session *config.AuthConfig
	// RelayManifest pins the reviewed tool definitions served by this relay.
	// Serve resolves its version again; caller-supplied definitions are not trusted.
	RelayManifest *ToolManifest
	// Confirm asks the user about allowed calls unless AllowAll or the
	// read-only AllowRead exemption applies. nil refuses calls that require
	// confirmation.
	Confirm func(ctx context.Context, tool string, args map[string]any) bool
	// Log receives one line per call: the tool and what happened to it.
	Log func(tool, outcome string)
}

func (s *mcpServer) setBindingPolicy(p *BindingPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.binding = p
}

func (s *mcpServer) bindingPolicy() *BindingPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.binding
}

func (p *BindingPolicy) allows(tool mcpgo.Tool) bool {
	return p.AllowAll || slices.Contains(p.Tools, tool.Name) || readOnlyTool(tool)
}

func (p *BindingPolicy) log(tool, outcome string) {
	if p.Log != nil {
		p.Log(tool, outcome)
	}
}

// hostTools reach this host or past the connected device: local paths and
// drives, simulators, LAN scans, explicit addresses and broker URLs. The relay
// can forge any call, so a bound client gets them only by --allow-tool or
// --allow-all. Each call is confirmed unless --allow-all was passed.
var hostTools = []string{"project_validate", "app_inspect", "device_list", "cloud_ping", "os_install_plan", "os_list_drives", "os_install_start", "os_install_status", "os_install_resume", "os_install_verify", "simulator_list", "simulator_create", "simulator_start", "simulator_update_agent", "simulator_viewer", "simulator_stop", "simulator_delete"}

// readOnlyTool identifies built-in reads that stay off the host. These tools
// are allowed by default, but skip confirmation only when AllowRead is set.
// Tools proxied from app containers describe themselves, so they never count.
func readOnlyTool(tool mcpgo.Tool) bool {
	builtin := false
	for _, names := range toolGroups {
		builtin = builtin || slices.Contains(names, tool.Name)
	}
	hint := tool.Annotations.ReadOnlyHint
	return builtin && !slices.Contains(hostTools, tool.Name) && hint != nil && *hint
}

// CheckToolNames refuses a name that is not a built-in tool, listing the valid
// ones, so a typo in --allow-tool fails at start instead of allowing nothing.
func CheckToolNames(names []string) error {
	var valid []string
	for _, group := range toolGroups {
		valid = append(valid, group...)
	}
	slices.Sort(valid)
	for _, name := range names {
		if !slices.Contains(valid, name) {
			return fmt.Errorf("unknown tool %q; valid tools: %s", name, strings.Join(valid, ", "))
		}
	}
	return nil
}

// otherSession returns the argument naming a cloud session other than the
// pinned one: a cloud_grpc endpoint, or a cloud:// device selector for
// another endpoint or organization.
func (p *BindingPolicy) otherSession(args map[string]any) (string, bool) {
	var own []string
	if p.Session != nil && len(p.Session.Certificates) > 0 {
		cert := p.Session.Certificates[0]
		own = append(own, strings.ToLower(fmt.Sprintf("cloud://%s/tenant/%s/", p.Session.CloudGRPC, cert.TenantUUID())))
		if cert.OrganizationID > 0 {
			own = append(own, strings.ToLower(fmt.Sprintf("cloud://%s/org/%d/", p.Session.CloudGRPC, cert.OrganizationID)))
		}
	}
	for key, value := range args {
		v, _ := value.(string)
		lower := strings.ToLower(v)
		switch {
		case key == "cloud_grpc" && v != "" && (p.Session == nil || v != p.Session.CloudGRPC):
			return key, true
		case strings.HasPrefix(lower, "cloud:") && !slices.ContainsFunc(own, func(prefix string) bool { return strings.HasPrefix(lower, prefix) }):
			return key, true
		}
	}
	return "", false
}

// session is the pinned session as config.json holds it now (tokens refresh
// on disk), while it still names the same endpoint and organization.
func (p *BindingPolicy) session(cfg *config.Config) (*config.AuthConfig, error) {
	if p.Session != nil && cfg != nil {
		cfg.EnsureContexts()
		if auth, ok := cfg.ContextByName(p.Session.Name); ok && auth.CloudGRPC == p.Session.CloudGRPC && auth.OrganizationKey() == p.Session.OrganizationKey() {
			return auth, nil
		}
	}
	return nil, &cloudResolveErr{code: errCodeDenied, msg: "the auth session this relay session started with is gone or changed organization; restart 'wendy cloud mcp'"}
}

// bindingMiddleware enforces the binding policy on every tool call.
func (s *mcpServer) bindingMiddleware(srv *server.MCPServer) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			p := s.bindingPolicy()
			if p == nil {
				return next(ctx, req)
			}
			name := req.Params.Name
			registered := srv.GetTool(name)
			if registered == nil || !p.allows(registered.Tool) {
				p.log(name, "refused: not allowed for this client")
				return errResultf(errCodeDenied, "%s is not allowed for this client; the user can allow it with 'wendy cloud mcp --allow-tool %s'", name, name), nil
			}
			// ponytail: top-level arguments only, which is where every built-in
			// takes cloud_grpc and device selectors. The wendy run child still
			// resolves a bare device_name from config.json; pin it with a
			// --context flag when the CLI grows one.
			if key, other := p.otherSession(req.GetArguments()); other {
				p.log(name, "refused: "+key+" names another session")
				return errResultf(errCodeDenied, "%s names another Wendy Cloud session or organization; this client may use only the one 'wendy cloud mcp' started with", key), nil
			}
			if !p.AllowAll && !(p.AllowRead && readOnlyTool(registered.Tool)) {
				if p.Confirm == nil || !p.Confirm(ctx, name, req.GetArguments()) {
					p.log(name, "refused by the user")
					return errResultf(errCodeDenied, "the user did not confirm %s", name), nil
				}
			}
			result, err := next(ctx, req)
			switch {
			case err != nil:
				p.log(name, "failed: "+err.Error())
			case result != nil && result.IsError:
				p.log(name, "returned an error")
			default:
				p.log(name, "ok")
			}
			return result, err
		}
	}
}

// allowMCPClaim is the wendy-auth claim carrying the realm's "Allow MCP"
// setting. ponytail: name from the Relay MCP contract draft (cloud-worker
// t-08); confirm when service-protos docs/relay-mcp.md lands.
const allowMCPClaim = "allow_mcp"

// RealmAllowsMCP refuses MCP for a session whose realm turned "Allow MCP" off.
// Only an explicit false refuses: the realm default is on, and older tokens
// and personal access tokens do not carry the claim. The token is decoded,
// not verified; Cloud enforces the setting on the channel it is sent.
func RealmAllowsMCP(auth *config.AuthConfig) error {
	if auth == nil {
		return nil
	}
	parts := strings.Split(auth.APIKey, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return nil
	}
	if allowed, ok := claims[allowMCPClaim].(bool); ok && !allowed {
		return fmt.Errorf("your organization does not allow MCP clients (session %q); ask an organization admin to turn on Allow MCP", auth.Name)
	}
	return nil
}
