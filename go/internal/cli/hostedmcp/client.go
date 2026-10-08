// Package hostedmcp connects the CLI to an organization's hosted gRPC proxy.
package hostedmcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Target struct{ Origin, Organization, Device string }

// Parse accepts an exact selector, so path aliases and URL credentials cannot
// redirect an OAuth token. Custom HTTPS origins support self-hosted installations.
func Parse(selector string) (Target, error) {
	u, err := url.Parse(selector)
	if err != nil || u.Scheme != "mcp" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return Target{}, fmt.Errorf("expected mcp://host/orgs/<UUID>/devices/<UUID>")
	}
	parts := strings.Split(u.EscapedPath(), "/")
	if len(parts) != 5 || parts[0] != "" || parts[1] != "orgs" || parts[3] != "devices" {
		return Target{}, fmt.Errorf("invalid hosted MCP selector")
	}
	for _, value := range []string{parts[2], parts[4]} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return Target{}, fmt.Errorf("hosted MCP requires canonical organization and device UUIDs")
		}
	}
	return Target{"https://" + u.Host, parts[2], parts[4]}, nil
}
func (t Target) Resource() string { return t.Origin + "/orgs/" + t.Organization + "/mcp" }

type TokenProvider func(context.Context, string) (string, error)

// Connect uses TLS on the WebSocket; gRPC itself runs inside that encrypted
// connection. Tokens are obtained for this exact resource on every reconnect.
func Connect(ctx context.Context, selector string, tokens TokenProvider) (*grpcclient.AgentConnection, error) {
	target, err := Parse(selector)
	if err != nil {
		return nil, err
	}
	if tokens == nil {
		return nil, fmt.Errorf("MCP OAuth token provider required")
	}
	lifetime, cancel := context.WithCancel(ctx)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	dial := func(dialCtx context.Context, _ string) (net.Conn, error) {
		token, err := tokens(dialCtx, target.Resource())
		if err != nil {
			return nil, err
		}
		if token == "" || strings.ContainsAny(token, "\r\n") {
			return nil, fmt.Errorf("missing MCP OAuth token")
		}
		endpoint := target.Origin + "/orgs/" + target.Organization + "/devices/" + target.Device + "/tunnel"
		ws, _, err := websocket.Dial(dialCtx, endpoint, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
		if err != nil {
			return nil, fmt.Errorf("connecting to hosted MCP: %w", err)
		}
		ws.SetReadLimit(4 << 20)
		return websocket.NetConn(lifetime, ws, websocket.MessageBinary), nil
	}
	conn, err := grpc.NewClient("passthrough:///hosted-mcp", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(dial), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(4<<20), grpc.MaxCallSendMsgSize(4<<20)))
	if err != nil {
		cancel()
		return nil, err
	}
	result := grpcclient.NewFromConn(conn)
	result.Host, result.Addr = selector, selector
	result.ExtraClosers = append(result.ExtraClosers, cancelCloser{cancel})
	result.RegistryDialer = func(ctx context.Context, port int) (net.Conn, error) {
		service := "wendy-registry"
		if port == 5555 {
			service = "wendy-registry-darwin"
		} else if port != 5000 {
			return nil, fmt.Errorf("registry port is not in the hosted service catalog")
		}
		return DialService(ctx, selector, service, tokens)
	}
	result.Reconnect = func(ctx context.Context) (*grpcclient.AgentConnection, error) { return Connect(ctx, selector, tokens) }
	probe, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	if _, err := result.AgentService.GetAgentVersion(probe, &agentpb.GetAgentVersionRequest{}); err != nil {
		result.Close()
		return nil, err
	}
	return result, nil
}

type cancelCloser struct{ cancel context.CancelFunc }

func (c cancelCloser) Close() error { c.cancel(); return nil }

// DialService carries a catalogued device service through the hosted gateway.
func DialService(ctx context.Context, selector, service string, tokens TokenProvider) (net.Conn, error) {
	if service != "ssh" && service != "wendy-registry" && service != "wendy-registry-darwin" {
		return nil, fmt.Errorf("unknown hosted service")
	}
	target, err := Parse(selector)
	if err != nil {
		return nil, err
	}
	token, err := tokens(ctx, target.Resource())
	if err != nil {
		return nil, err
	}
	endpoint := target.Origin + "/orgs/" + target.Organization + "/devices/" + target.Device + "/services/" + service + "/tunnel"
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ws, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(1 << 20)
	return websocket.NetConn(ctx, ws, websocket.MessageBinary), nil
}
