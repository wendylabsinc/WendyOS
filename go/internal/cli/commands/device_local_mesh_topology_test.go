package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var connectLocalMeshTopology = connectToAgent

func topologyResolverFixture(t *testing.T) {
	previous := resolveLocalMeshTopologyTarget
	resolveLocalMeshTopologyTarget = func(ctx context.Context, options ...resolveOption) (*SelectedDevice, error) {
		conn, err := connectLocalMeshTopology(ctx, options...)
		if err != nil {
			return nil, err
		}
		return &SelectedDevice{Agent: conn}, nil
	}
	t.Cleanup(func() { resolveLocalMeshTopologyTarget = previous })
}
func TestLocalMeshTopologyReadsOnlySelectedAgent(t *testing.T) {
	topologyResolverFixture(t)
	const principal = "spiffe://wendy.sh/tenant/5c9e14bb-49c0-4178-9cc5-493a151f9885/device/b1026a08-24e8-4563-acf8-4138cb5986cb"
	identity, err := certs.ParsePrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	device, err := meshid.DeviceFromPrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	for _, rpcCode := range []codes.Code{codes.OK, codes.Unimplemented, codes.PermissionDenied} {
		t.Run(rpcCode.String(), func(t *testing.T) {
			calls := 0
			connection, err := grpc.NewClient("passthrough:///selected-mesh-agent",
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithUnaryInterceptor(func(ctx context.Context, method string, request, reply any, _ *grpc.ClientConn, _ grpc.UnaryInvoker, _ ...grpc.CallOption) error {
					calls++
					if method != pb.WendyLocalMeshAdminService_GetLocalMeshTopology_FullMethodName {
						t.Fatalf("unexpected RPC or mutation: %s", method)
					}
					if _, ok := request.(*pb.GetLocalMeshTopologyRequest); !ok {
						t.Fatalf("unexpected request: %T", request)
					}
					deadline, bounded := ctx.Deadline()
					if !bounded || time.Until(deadline) > localMeshTopologyTimeout {
						t.Fatal("snapshot request has no bounded deadline")
					}
					if rpcCode != codes.OK {
						return status.Error(rpcCode, "snapshot unavailable")
					}
					proto.Merge(reply.(proto.Message), &pb.LocalMeshTopology{
						SelfPrincipal: principal, SelfTenantId: device.Tenant().UUID().String(),
						SelfDeviceId: device.UUID().String(), SelfMeshAddress: device.MeshAddress().String(),
						CapturedAtUnixMs: 1792342800123, RuntimeAvailable: false,
						Status: &pb.LocalMeshStatus{State: "disabled"}, Truncated: true,
					})
					return nil
				}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = connection.Close() })
			previousConnect, previousJSON, previousIdentity := connectLocalMeshTopology, jsonOutput, observedLocalMeshTopologyIdentity
			t.Cleanup(func() {
				connectLocalMeshTopology, jsonOutput, observedLocalMeshTopologyIdentity = previousConnect, previousJSON, previousIdentity
			})
			observedLocalMeshTopologyIdentity = func(*grpcclient.AgentConnection) (certs.WendyIdentity, bool) {
				return identity, true
			}
			connectLocalMeshTopology = func(_ context.Context, options ...resolveOption) (*grpcclient.AgentConnection, error) {
				cfg := resolveConfig{}
				for _, option := range options {
					option(&cfg)
				}
				if !cfg.readOnlyMonitoring || !cfg.suppressUpdateCheck || !cfg.disablePickerEnroll || !cfg.nonInteractive {
					t.Fatal("topology connection permits lifecycle changes or interactive target substitution")
				}
				return &grpcclient.AgentConnection{Conn: connection}, nil
			}
			jsonOutput = true
			root := newDeviceLocalMeshCmd()
			root.SetContext(context.Background())
			root.SetArgs([]string{"topology"})
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			root.SilenceUsage, root.SilenceErrors = true, true
			err = root.Execute()
			if calls != 1 || connection.GetState() != connectivity.Shutdown {
				t.Fatalf("calls=%d transport=%v", calls, connection.GetState())
			}
			if rpcCode != codes.OK {
				if status.Code(err) != rpcCode || output.Len() != 0 {
					t.Fatalf("RPC failure became a successful/empty topology: err=%v output=%s", err, output.String())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var snapshot map[string]any
			if err := json.Unmarshal(output.Bytes(), &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot["selfPrincipal"] != principal || snapshot["capturedAtUnixMs"] != "1792342800123" || snapshot["runtimeAvailable"] != false || snapshot["truncated"] != true {
				t.Fatalf("lost snapshot identity, ProtoJSON precision or unavailable/truncated state: %s", output.String())
			}
			if _, ok := snapshot["routes"].([]any); !ok {
				t.Fatalf("empty route list omitted from JSON: %s", output.String())
			}
		})
	}
}

func TestLocalMeshTopologyObservationClassifiesConnectionErrors(t *testing.T) {
	topologyResolverFixture(t)
	for _, test := range []struct {
		name   string
		err    error
		status string
	}{
		{"unsupported", status.Error(codes.Unimplemented, "older agent"), "unsupported"},
		{"offline", status.Error(codes.Unavailable, "offline"), "unavailable"},
		{"deadline", context.DeadlineExceeded, "unavailable"},
		{"denied", status.Error(codes.PermissionDenied, "denied"), "unauthorized"},
		{"unauthenticated", status.Error(codes.Unauthenticated, "unverified"), "unauthorized"},
		{"unknown", errors.New("unavailable unauthorized unsupported words are not status codes"), "error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			previousConnect, previousJSON := connectLocalMeshTopology, jsonOutput
			t.Cleanup(func() { connectLocalMeshTopology, jsonOutput = previousConnect, previousJSON })
			connectLocalMeshTopology = func(context.Context, ...resolveOption) (*grpcclient.AgentConnection, error) {
				return nil, test.err
			}
			jsonOutput = true
			cmd := newDeviceLocalMeshTopologyCmd()
			cmd.SetArgs([]string{"--observation"})
			var output bytes.Buffer
			cmd.SetOut(&output)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result["status"] != test.status || result["topology"] != nil || result["error"] == "" || result["error"] == nil {
				t.Fatalf("incorrect typed failure: %s", output.String())
			}
			if _, err := time.Parse(time.RFC3339Nano, result["observedAt"].(string)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLocalMeshTopologyRejectsUnverifiedReporter(t *testing.T) {
	topologyResolverFixture(t)
	connection, err := grpc.NewClient("passthrough:///unverified-agent",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(func(_ context.Context, _ string, _, reply any, _ *grpc.ClientConn, _ grpc.UnaryInvoker, _ ...grpc.CallOption) error {
			proto.Merge(reply.(proto.Message), &pb.LocalMeshTopology{SelfPrincipal: "untrusted claimed identity"})
			return nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	previous := connectLocalMeshTopology
	t.Cleanup(func() { connectLocalMeshTopology = previous })
	connectLocalMeshTopology = func(context.Context, ...resolveOption) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{Conn: connection}, nil
	}
	if snapshot, _, err := readLocalMeshReporter(context.Background()); snapshot != nil || status.Code(err) != codes.Unauthenticated {
		t.Fatalf("accepted unauthenticated snapshot: %v, %v", snapshot, err)
	}
}

func TestLocalMeshTopologyObservationRequiresJSONBeforeConnecting(t *testing.T) {
	topologyResolverFixture(t)
	previousConnect, previousJSON := connectLocalMeshTopology, jsonOutput
	t.Cleanup(func() { connectLocalMeshTopology, jsonOutput = previousConnect, previousJSON })
	jsonOutput = false
	connectLocalMeshTopology = func(context.Context, ...resolveOption) (*grpcclient.AgentConnection, error) {
		t.Fatal("connected despite invalid output flags")
		return nil, nil
	}
	cmd := newDeviceLocalMeshTopologyCmd()
	cmd.SetArgs([]string{"--observation"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil {
		t.Fatal("accepted --observation without --json")
	}
}

type liteTopologyFixtureProvider struct {
	providers.DeviceProvider
	calls int
}

func (p *liteTopologyFixtureProvider) NetTopology(ctx context.Context, device models.ExternalDevice) (*pb.LocalMeshTopology, error) {
	p.calls++
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < 130*time.Second {
		return nil, errors.New("Lite inherited the OS setup deadline")
	}
	if device.ID != "wendy-lite:physical" {
		return nil, errors.New("wrong provider device")
	}
	return &pb.LocalMeshTopology{SelfDeviceId: "selected-lite"}, nil
}
func TestLiteTopologyUsesExistingProviderCapability(t *testing.T) {
	previous := resolveLocalMeshTopologyTarget
	defer func() { resolveLocalMeshTopologyTarget = previous }()
	provider := &liteTopologyFixtureProvider{}
	resolveLocalMeshTopologyTarget = func(_ context.Context, options ...resolveOption) (*SelectedDevice, error) {
		cfg := resolveConfig{}
		for _, option := range options {
			option(&cfg)
		}
		if !cfg.readOnlyMonitoring {
			t.Fatal("observation can mutate lifecycle")
		}
		return &SelectedDevice{External: &models.ExternalDevice{ID: "wendy-lite:physical"}, Provider: provider}, nil
	}
	snapshot, platform, err := readLocalMeshReporter(context.Background())
	if err != nil || platform != "wendy-lite" || snapshot.SelfDeviceId != "selected-lite" || provider.calls != 1 {
		t.Fatal(snapshot, platform, provider.calls, err)
	}
}
