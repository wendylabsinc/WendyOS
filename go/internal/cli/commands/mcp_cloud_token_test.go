package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// staleTokenAssetServer is a v2 AssetService that, like Cloud, refuses an
// expired access token and serves one device to any other DPoP token.
type staleTokenAssetServer struct {
	cloudpbv2.UnimplementedAssetServiceServer
	stale string

	mu   sync.Mutex
	seen []string
}

func (s *staleTokenAssetServer) ListAssets(_ *cloudpbv2.ListAssetsRequest, stream cloudpbv2.AssetService_ListAssetsServer) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	authorization := strings.Join(md.Get("authorization"), ",")
	s.mu.Lock()
	s.seen = append(s.seen, authorization)
	s.mu.Unlock()
	if !strings.HasPrefix(authorization, "DPoP ") || authorization == "DPoP "+s.stale {
		return status.Error(codes.Unauthenticated, "authentication required")
	}
	return stream.Send(&cloudpbv2.ListAssetsResponse{
		Asset: &cloudpbv2.Asset{Id: "11111111-1111-4111-8111-111111111111", Name: "edge-one", IsComputeDevice: true},
		Total: 1,
	})
}

// callMCPTool runs one tool call through srv's MCP protocol stream, as
// `wendy cloud mcp` serves it, and returns the call's result.
func callMCPTool(t *testing.T, srv *wendymcp.Server, policy *wendymcp.BindingPolicy, tool string) map[string]any {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- srv.Serve(ctx, policy, inR, outW)
		_ = outW.Close()
	}()
	go func() {
		for _, msg := range []string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, tool),
		} {
			if _, err := io.WriteString(inW, msg+"\n"); err != nil {
				return
			}
		}
	}()
	lines := bufio.NewScanner(outR)
	lines.Buffer(make([]byte, 1<<20), 1<<20)
	var result map[string]any
	for result == nil && lines.Scan() {
		var msg struct {
			ID     int            `json:"id"`
			Result map[string]any `json:"result"`
		}
		if json.Unmarshal(lines.Bytes(), &msg) == nil && msg.ID == 2 {
			result = msg.Result
		}
	}
	_ = inW.Close()
	cancel()
	<-served
	if result == nil {
		t.Fatalf("no result for %s", tool)
	}
	return result
}

// `wendy mcp serve` and `wendy cloud mcp` outlive wendy-auth's one-hour access
// token. Their cloud tools renew it as terminal commands do, instead of
// sending the expired token Cloud refuses.
func TestMCPServerRenewsExpiredCloudToken(t *testing.T) {
	t.Setenv("WENDY_NO_UPDATE_CHECK", "1")
	t.Setenv("WENDY_ANALYTICS", "false")
	t.Setenv("WENDY_DEVICE", "")
	t.Setenv("WENDY_AGENT_SOCKET", "")
	auth, refreshes := rotatingOAuthSession(t)
	cloud := &staleTokenAssetServer{stale: auth.APIKey}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	cloudpbv2.RegisterAssetServiceServer(grpcServer, cloud)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	// Make the stored session a v2 operator session on the fake Cloud.
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(auth.DPoPPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Update(func(cfg *config.Config) (bool, error) {
		cfg.Auth[0].CloudGRPC = lis.Addr().String()
		cfg.Auth[0].Certificates = []config.CertificateInfo{{PemCertificate: testLeafPEM(t, key), PemPrivateKey: auth.DPoPPrivateKey}}
		cfg.EnsureContexts()
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	session := cfg.Auth[0]

	srv, err := newMCPServer("", []string{"all"})
	if err != nil {
		t.Fatal(err)
	}
	result := callMCPTool(t, srv, &wendymcp.BindingPolicy{AllowAll: true, Session: &session}, "cloud_discover")
	if isError, _ := result["isError"].(bool); isError {
		t.Fatalf("cloud_discover failed: %v", result["content"])
	}
	devices, _ := result["structuredContent"].(map[string]any)["devices"].([]any)
	if len(devices) != 1 {
		t.Fatalf("devices = %v, want edge-one", result["structuredContent"])
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refreshed %d times, want once", refreshes.Load())
	}
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	for _, got := range cloud.seen {
		if got == "DPoP "+auth.APIKey {
			t.Fatalf("Cloud was sent the expired token: %v", cloud.seen)
		}
	}
}
