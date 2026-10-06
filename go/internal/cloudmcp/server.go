// Package cloudmcp serves organization-scoped MCP using a Cloud authorization
// backend and service-account connections to devices.
package cloudmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	_ "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	_ "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

const (
	ProductionURL = "https://mcp.wendy.dev"
	TestURL       = "https://mcp.dev.wendy.sh"
	maxBody       = 1 << 20
)

// Access is a live decision from the Cloud control plane, after it verifies the
// OAuth token and resolves both principals' current permissions. No field may
// be populated from unverified token claims or caller-provided identity headers.
type Access struct {
	OwnerPrincipal string   `json:"owner_principal"`
	DelegationID   string   `json:"delegation_id"`
	Entitlements   []string `json:"entitlements"`
	bearer         string   // stays inside the trusted backend; never serialized

	OrganizationID string    `json:"organization_id"`
	TenantID       string    `json:"tenant_id"`
	UserID         string    `json:"user_id"`
	ServiceSubject string    `json:"service_subject"`
	DecisionID     string    `json:"decision_id"`
	Enabled        bool      `json:"enabled"`
	UserAllowed    bool      `json:"user_allowed"`
	ServiceAllowed bool      `json:"service_allowed"`
	ExpiresAt      time.Time `json:"expires_at"`
}

func (a Access) sameIdentity(b Access) bool {
	return a.OrganizationID == b.OrganizationID && a.TenantID == b.TenantID && a.UserID == b.UserID && a.ServiceSubject == b.ServiceSubject && a.DelegationID == b.DelegationID
}

func (a Access) permits(org string) bool {
	return a.OrganizationID == org && canonicalUUID(a.TenantID) && a.UserID != "" && a.ServiceSubject != "" && a.Enabled && a.UserAllowed && a.ServiceAllowed && time.Now().Before(a.ExpiresAt)
}

type Organization struct {
	Issuer string `json:"issuer"`
}

// Backend is part of the MCP resource server's trusted authorization boundary.
// The user's OAuth token is verified here, never sent to a device or exchanged
// for the organization's service credential. Checks must fail on disabled
// organizations, revoked identities, and unresolved permissions.
type Backend interface {
	Organization(context.Context, string) (Organization, error)
	Authorize(ctx context.Context, token, organization, device, method string) (Access, error)
	Devices(ctx context.Context, access Access) (json.RawMessage, error)
	Record(context.Context, Access, GatewayEvent) error
}

// Connector must authenticate as access.ServiceSubject in access.TenantID and
// verify the selected device's tenant and device ID during its mTLS handshake.
// It must route through Cloud's authorized tunnel catalog, never a caller's host.
type Connector interface {
	Connect(ctx context.Context, access Access, device string) (*grpcclient.AgentConnection, error)
}

type Server struct {
	base      *url.URL
	backend   Backend
	connector Connector
	protocol  *server.StreamableHTTPServer
	logger    *slog.Logger
	tracer    trace.Tracer
	slots     chan struct{}
}

type caller struct {
	token, org string
	access     Access
}
type callerKey struct{}

func New(baseURL string, backend Backend, connector Connector, options ...func(*Server)) (*Server, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.Path != "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || backend == nil || connector == nil {
		return nil, fmt.Errorf("MCP requires an explicit HTTPS origin, authorization backend, and device connector")
	}
	s := &Server{base: base, backend: backend, connector: connector, logger: slog.Default(), tracer: otel.Tracer("wendy.cloudmcp"), slots: make(chan struct{}, 128)}
	for _, option := range options {
		option(s)
	}
	protocol := server.NewMCPServer("wendy-cloud", "1.0.0", server.WithToolCapabilities(false), server.WithHooks(s.protocolHooks()))
	protocol.AddTool(mcp.NewTool("device_list", mcp.WithDescription("List devices accessible to both you and this organization's MCP service account."), mcp.WithReadOnlyHintAnnotation(true)), s.observedTool("device_list", s.listDevices))
	protocol.AddTool(mcp.NewTool("device_methods", mcp.WithDescription("Describe a Wendy Agent RPC's request fields, or list supported RPC methods."), mcp.WithString("method"), mcp.WithReadOnlyHintAnnotation(true)), s.observedTool("device_methods", s.describeMethods))
	protocol.AddTool(mcp.NewTool("device_rpc", mcp.WithDescription("Call a Wendy Agent RPC through the organization's service account. Both your current permissions and the service account's permissions must allow it. Client-streaming RPCs use the CLI tunnel."), mcp.WithString("device_id", mcp.Required()), mcp.WithString("method", mcp.Required()), mcp.WithObject("request"), mcp.WithNumber("max_messages"), mcp.WithNumber("timeout_seconds"), mcp.WithReadOnlyHintAnnotation(false), mcp.WithDestructiveHintAnnotation(true), mcp.WithOpenWorldHintAnnotation(true)), s.observedTool("device_rpc", s.callDevice))
	s.protocol = server.NewStreamableHTTPServer(protocol, server.WithStateLess(true))
	return s, nil
}

func canonicalUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Host != s.base.Host {
		http.Error(w, "invalid host", http.StatusBadRequest)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != s.base.String() {
		http.Error(w, "invalid origin", http.StatusForbidden)
		return
	}
	path := r.URL.EscapedPath()
	metadata := strings.HasPrefix(path, "/.well-known/oauth-protected-resource")
	if metadata {
		path = strings.TrimPrefix(path, "/.well-known/oauth-protected-resource")
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	isMCP := len(parts) == 3 && parts[0] == "orgs" && canonicalUUID(parts[1]) && parts[2] == "mcp" && path == "/orgs/"+parts[1]+"/mcp"
	isTunnel := !metadata && len(parts) == 5 && parts[0] == "orgs" && canonicalUUID(parts[1]) && parts[2] == "devices" && canonicalUUID(parts[3]) && parts[4] == "tunnel" && path == "/orgs/"+parts[1]+"/devices/"+parts[3]+"/tunnel"
	isService := !metadata && len(parts) == 7 && parts[0] == "orgs" && canonicalUUID(parts[1]) && parts[2] == "devices" && canonicalUUID(parts[3]) && parts[4] == "services" && allowedService(parts[5]) && parts[6] == "tunnel" && path == "/orgs/"+parts[1]+"/devices/"+parts[3]+"/services/"+parts[5]+"/tunnel"
	if !isMCP && !isTunnel && !isService {
		http.NotFound(w, r)
		return
	}
	org := parts[1]
	resourcePath := "/orgs/" + org + "/mcp"
	resource := s.base.String() + resourcePath
	if metadata {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		organization, err := s.backend.Organization(r.Context(), org)
		issuer, parseErr := url.Parse(organization.Issuer)
		if err != nil || parseErr != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": resource, "authorization_servers": []string{organization.Issuer}, "bearer_methods_supported": []string{"header"}, "scopes_supported": []string{"mcp:read", "mcp:control"}})
		return
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") || len(values[0]) > 32768 || strings.TrimSpace(strings.TrimPrefix(values[0], "Bearer ")) == "" {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+s.base.String()+"/.well-known/oauth-protected-resource"+resourcePath+`"`)
		http.Error(w, "OAuth authentication required", http.StatusUnauthorized)
		return
	}
	c := caller{token: strings.TrimPrefix(values[0], "Bearer "), org: org}
	r = r.WithContext(context.WithValue(r.Context(), callerKey{}, c))
	device, operation := "", "mcp.connect"
	if isTunnel {
		device, operation = parts[3], "cli.connect"
	}
	if isService {
		device, operation = parts[3], "service:"+parts[5]
	}
	ctx, finishAuth := s.observe(r.Context(), "mcp.authorization", attribute.String("wendy.operation", operation))
	access, err := s.backend.Authorize(ctx, c.token, org, device, operation)
	if err != nil || !access.permits(org) {
		finishAuth("denied")
	} else {
		finishAuth("ok")
	}
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+s.base.String()+"/.well-known/oauth-protected-resource"+resourcePath+`"`)
			http.Error(w, "authentication failed", http.StatusUnauthorized)
		} else {
			http.Error(w, "authorization service unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	if !access.permits(org) {
		http.Error(w, "organization access denied", http.StatusForbidden)
		return
	}
	c.access = access
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	r = r.WithContext(context.WithValue(r.Context(), callerKey{}, c))
	if isService {
		s.serveServiceTunnel(w, r, device, parts[5], access)
		return
	}
	if isTunnel {
		s.serveTunnel(w, r, device, access)
		return
	}
	s.protocol.ServeHTTP(w, r)
}

func (s *Server) authorize(ctx context.Context, device, method string) (Access, error) {
	c, ok := ctx.Value(callerKey{}).(caller)
	if !ok {
		return Access{}, errors.New("missing authenticated caller")
	}
	ctx, end := s.observe(ctx, "mcp.authorization", attribute.String("wendy.operation", method), attribute.String("wendy.device_id", device))
	a, err := s.backend.Authorize(ctx, c.token, c.org, device, method)
	if err != nil || !a.permits(c.org) {
		end("denied")
		return Access{}, errors.New("operation denied")
	}
	end("ok")
	return a, nil
}

func result(value any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(b)), nil
}

func (s *Server) listDevices(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	a, err := s.authorize(ctx, "", "device_list")
	if err != nil {
		return mcp.NewToolResultError("device access denied"), nil
	}
	devices, err := s.backend.Devices(ctx, a)
	if err != nil || len(devices) > maxBody || !json.Valid(devices) {
		return mcp.NewToolResultError("device inventory unavailable"), nil
	}
	return mcp.NewToolResultText(string(devices)), nil
}

func agentMethod(name string) (protoreflect.MethodDescriptor, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 3 || parts[0] != "" {
		return nil, errors.New("use /package.Service/Method")
	}
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(parts[1]))
	if err != nil {
		return nil, errors.New("unknown agent service")
	}
	service, ok := d.(protoreflect.ServiceDescriptor)
	if !ok || !strings.HasPrefix(string(service.FullName()), "wendy.agent.services.") {
		return nil, errors.New("not an agent service")
	}
	method := service.Methods().ByName(protoreflect.Name(parts[2]))
	if method == nil {
		return nil, errors.New("unknown agent method")
	}
	return method, nil
}

func (s *Server) describeMethods(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	access, err := s.authorize(ctx, "", "device_methods")
	if err != nil {
		return mcp.NewToolResultError("access denied"), nil
	}
	name := request.GetString("method", "")
	if name == "" {
		methods := []string{}
		protoregistry.GlobalFiles.RangeFiles(func(file protoreflect.FileDescriptor) bool {
			if !strings.HasPrefix(string(file.Package()), "wendy.agent.services.") {
				return true
			}
			for i := 0; i < file.Services().Len(); i++ {
				service := file.Services().Get(i)
				for j := 0; j < service.Methods().Len(); j++ {
					name := "/" + string(service.FullName()) + "/" + string(service.Methods().Get(j).Name())
					if access.DelegationID == "" || permitsMethod(access, name) {
						methods = append(methods, name)
					}
				}
			}
			return true
		})
		sort.Strings(methods)
		return result(methods)
	}
	if access.DelegationID != "" && !permitsMethod(access, name) {
		return mcp.NewToolResultError("method is outside delegation"), nil
	}
	method, err := agentMethod(name)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	fields := []map[string]any{}
	for i := 0; i < method.Input().Fields().Len(); i++ {
		f := method.Input().Fields().Get(i)
		fields = append(fields, map[string]any{"name": f.JSONName(), "type": f.Kind().String(), "repeated": f.IsList(), "map": f.IsMap()})
	}
	return result(map[string]any{"method": name, "request_fields": fields, "client_streaming": method.IsStreamingClient(), "server_streaming": method.IsStreamingServer()})
}

func (s *Server) callDevice(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	device, name := request.GetString("device_id", ""), request.GetString("method", "")
	if !canonicalUUID(device) {
		return mcp.NewToolResultError("device_id must be a canonical UUID"), nil
	}
	method, err := agentMethod(name)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if method.IsStreamingClient() {
		return mcp.NewToolResultError("use the CLI tunnel for client-streaming RPCs"), nil
	}
	seconds := request.GetInt("timeout_seconds", 30)
	limit := request.GetInt("max_messages", 20)
	if seconds < 1 || seconds > 60 || limit < 1 || limit > 100 {
		return mcp.NewToolResultError("timeout_seconds must be 1..60 and max_messages 1..100"), nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	a, err := s.authorize(ctx, device, name)
	if err != nil {
		return mcp.NewToolResultError("device operation denied"), nil
	}
	ctx, cancelExpiry := context.WithDeadline(ctx, a.ExpiresAt)
	defer cancelExpiry()
	ctx, stopAuthorization := s.watchAuthorization(ctx, device, name, a)
	defer stopAuthorization()
	body := request.GetArguments()["request"]
	if body == nil {
		body = map[string]any{}
	}
	encoded, err := json.Marshal(body)
	if err != nil || len(encoded) > maxBody {
		return mcp.NewToolResultError("invalid RPC request"), nil
	}
	in := dynamicpb.NewMessage(method.Input())
	if err := protojson.Unmarshal(encoded, in); err != nil {
		return mcp.NewToolResultError("RPC request does not match the method schema"), nil
	}
	ctx, finishRPC := s.observe(ctx, "mcp.device_rpc", attribute.String("rpc.method", name), attribute.String("wendy.device_id", device))
	rpcOutcome := "error"
	defer func() { finishRPC(rpcOutcome) }()
	conn, closeConnection, err := s.connect(ctx, a, device)
	if err != nil {
		return mcp.NewToolResultError("device connection unavailable"), nil
	}
	defer closeConnection()
	if !method.IsStreamingServer() {
		out := dynamicpb.NewMessage(method.Output())
		if err := conn.Conn.Invoke(deviceTraceContext(ctx), name, in, out); err != nil {
			return mcp.NewToolResultError("device RPC failed"), nil
		}
		data, err := protojson.Marshal(out)
		if err != nil || len(data) > maxBody {
			return mcp.NewToolResultError("device response exceeds the MCP limit"), nil
		}
		rpcOutcome = "ok"
		return mcp.NewToolResultText(string(data)), nil
	}
	stream, err := conn.Conn.NewStream(deviceTraceContext(ctx), &grpc.StreamDesc{ServerStreams: true}, name)
	if err != nil {
		return mcp.NewToolResultError("device stream unavailable"), nil
	}
	if err = stream.SendMsg(in); err != nil {
		return mcp.NewToolResultError("device stream failed"), nil
	}
	if err = stream.CloseSend(); err != nil {
		return mcp.NewToolResultError("device stream failed"), nil
	}
	messages := []json.RawMessage{}
	size := 0
	for len(messages) < limit {
		if current, err := s.authorize(ctx, device, name); err != nil || !current.sameIdentity(a) {
			return mcp.NewToolResultError("device operation no longer authorized"), nil
		}
		out := dynamicpb.NewMessage(method.Output())
		err := stream.RecvMsg(out)
		if errors.Is(err, io.EOF) {
			rpcOutcome = "ok"
			return result(map[string]any{"messages": messages, "truncated": false})
		}
		if err != nil {
			return mcp.NewToolResultError("device stream failed"), nil
		}
		data, err := protojson.Marshal(out)
		size += len(data)
		if err != nil || size > maxBody {
			rpcOutcome = "truncated"
			return result(map[string]any{"messages": messages, "truncated": true})
		}
		messages = append(messages, data)
	}
	rpcOutcome = "truncated"
	return result(map[string]any{"messages": messages, "truncated": true})
}

func permitsMethod(a Access, method string) bool {
	parts := strings.Split(strings.TrimPrefix(method, "/"), "/")
	if len(parts) != 2 {
		return false
	}
	for _, e := range a.Entitlements {
		if e == "entitlement:"+parts[0]+":"+parts[1]+":allow" {
			return true
		}
	}
	return false
}
