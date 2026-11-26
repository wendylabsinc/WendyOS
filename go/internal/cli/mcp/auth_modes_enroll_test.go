package mcp

import (
	"context"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/cli/closedtest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/localkey"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// A24 MCP entry: real tool dispatch and access/TLS service, a valid synthetic
// operator session and a Cloud request counter. No live Cloud credentials.
func TestAuthModesA24MCPBeforeCloud(t *testing.T) {
	t.Setenv("WENDY_CLOUD_INSECURE", "true")
	for _, role := range []trustedkeys.Role{trustedkeys.RoleOperator, trustedkeys.RoleViewer, trustedkeys.RoleNone} {
		t.Run(role.String(), func(t *testing.T) {
			owner, err := localkey.LoadOrCreate(filepath.Join(t.TempDir(), localkey.FileName))
			if err != nil {
				t.Fatal(err)
			}
			caller, err := localkey.LoadOrCreate(filepath.Join(t.TempDir(), localkey.FileName))
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			keys, err := trustedkeys.Open(filepath.Join(dir, trustedkeys.FileName))
			if err != nil {
				t.Fatal(err)
			}
			spki, err := localkey.PublicKeyDER(owner)
			if err != nil {
				t.Fatal(err)
			}
			o, err := keys.Close(spki, "fixture owner", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			spki, err = localkey.PublicKeyDER(caller)
			if err != nil {
				t.Fatal(err)
			}
			if role != trustedkeys.RoleNone {
				if _, err = keys.Add(spki, role, "fixture caller", o.Fingerprint(), time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if err = keys.SetKeylessApps(true, o.Fingerprint()); err != nil {
				t.Fatal(err)
			}
			supported := true
			provisioning := &enrollmentReadinessServer{state: &pb.NotProvisionedResponse{AcmeEnrollmentSupported: &supported}}
			d := closedtest.Start(t, dir, func(s *grpc.Server) { pb.RegisterWendyProvisioningServiceServer(s, provisioning) })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var conn *grpcclient.AgentConnection
			if role == trustedkeys.RoleNone {
				g, e := grpc.NewClient(d.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
				if e != nil {
					t.Fatal(e)
				}
				conn = &grpcclient.AgentConnection{Conn: g}
			} else {
				host, port, _ := net.SplitHostPort(d.Addr)
				n, _ := strconv.Atoi(port)
				conn, err = grpcclient.ConnectWithLocalKey(ctx, net.JoinHostPort(host, strconv.Itoa(n+1)), caller, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { conn.Close() })
			var requests atomic.Int32
			cloud := grpc.NewServer(grpc.UnknownServiceHandler(func(any, grpc.ServerStream) error {
				requests.Add(1)
				return status.Error(codes.PermissionDenied, "inert Cloud counter")
			}))
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go cloud.Serve(ln)
			t.Cleanup(cloud.Stop)
			auth := v2TestAuth(t, ln.Addr().String())
			s := New(&config.Config{Auth: []config.AuthConfig{*auth}}, nil)
			s.SetConn(conn)
			result, err := s.callTool(ctx, "cloud_enroll_device", map[string]any{"name": "auth-modes-inert"})
			if err != nil {
				t.Fatal(err)
			}
			message, _ := structuredMap(t, result)["message"].(string)
			if !result.IsError || !strings.Contains(message, "owner key") || requests.Load() != 0 || provisioning.started {
				t.Fatalf("MCP ownership gate: error=%t diagnostic=%q Cloud=%d deviceStarted=%t", result.IsError, message, requests.Load(), provisioning.started)
			}
			t.Log("MCP normal tool dispatch: owner diagnostic, zero Cloud requests, no provisioning start")
		})
	}
}
