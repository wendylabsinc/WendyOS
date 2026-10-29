package commands

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// renameLog records agent and Cloud calls in the order the command made them.
type renameLog struct {
	mu     sync.Mutex
	events []string
}

func (l *renameLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, fmt.Sprintf(format, args...))
}

func (l *renameLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.events, "\n")
}

type renameAgent struct {
	versionOnlyAgent
	log *renameLog
}

func (a renameAgent) SetHostname(_ context.Context, req *agentpb.SetHostnameRequest) (*agentpb.SetHostnameResponse, error) {
	a.log.add("agent SetHostname %s", req.GetHostname())
	return &agentpb.SetHostnameResponse{}, nil
}

// renameCloudV2 is the v2 broker: it serves only wendycloud.v2.AssetService,
// so a v1 call fails with Unimplemented as it does in production.
type renameCloudV2 struct {
	cloudpbv2.UnimplementedAssetServiceServer
	log       *renameLog
	asset     *cloudpbv2.Asset
	updateErr error
}

func (s *renameCloudV2) GetAsset(_ context.Context, req *cloudpbv2.GetAssetRequest) (*cloudpbv2.Asset, error) {
	s.log.add("cloud v2 GetAsset org=%s device=%s", req.GetOrganizationId(), req.GetDeviceId())
	return s.asset, nil
}

func (s *renameCloudV2) UpdateAsset(_ context.Context, in *cloudpbv2.SignedRequest) (*cloudpbv2.Asset, error) {
	var req cloudpbv2.UpdateAssetRequest
	if in.GetPayloadType() != "wendycloud.v2.UpdateAssetRequest" || proto.Unmarshal(in.GetPayload(), &req) != nil {
		return nil, status.Error(codes.PermissionDenied, "payload_type is not the method's request")
	}
	var claims struct {
		Operation string `json:"operation"`
		Target    struct {
			Resource string `json:"resource"`
		} `json:"target"`
	}
	parts := strings.Split(string(in.GetSignature()), ".")
	if len(parts) != 3 {
		return nil, status.Error(codes.PermissionDenied, "the operator request signature was rejected")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, status.Error(codes.PermissionDenied, "the operator request signature was rejected")
	}
	s.log.add("cloud v2 UpdateAsset id=%s name=%s op=%s resource=%s", req.GetId(), req.GetName(), claims.Operation, claims.Target.Resource)
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	return &cloudpbv2.Asset{Id: req.GetId(), OrganizationId: s.asset.GetOrganizationId(), Name: req.GetName()}, nil
}

type renameCloudV1 struct {
	cloudpb.UnimplementedAssetServiceServer
	log *renameLog
}

func (s *renameCloudV1) UpdateAsset(_ context.Context, req *cloudpb.UpdateAssetRequest) (*cloudpb.Asset, error) {
	s.log.add("cloud v1 UpdateAsset id=%d name=%s", req.GetId(), req.GetName())
	return &cloudpb.Asset{Id: req.GetId(), Name: req.GetName()}, nil
}

type renameProvisioningV1 struct {
	agentpb.UnimplementedWendyProvisioningServiceServer
	prov *agentpb.ProvisionedResponse
}

func (s renameProvisioningV1) IsProvisioned(context.Context, *agentpb.IsProvisionedRequest) (*agentpb.IsProvisionedResponse, error) {
	return &agentpb.IsProvisionedResponse{Response: &agentpb.IsProvisionedResponse_Provisioned{Provisioned: s.prov}}, nil
}

type renameProvisioningV2 struct {
	agentpbv2.UnimplementedWendyProvisioningServiceServer
	prov *agentpbv2.ProvisionedResponse
}

func (s renameProvisioningV2) IsProvisioned(context.Context, *agentpbv2.IsProvisionedRequest) (*agentpbv2.IsProvisionedResponse, error) {
	return &agentpbv2.IsProvisionedResponse{ResponseType: &agentpbv2.IsProvisionedResponse_Provisioned{Provisioned: s.prov}}, nil
}

func startRenameCloud(t *testing.T, register func(*grpc.Server)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	register(server)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

func runDeviceRename(t *testing.T, name string) (string, error) {
	t.Helper()
	device, asJSON, fresh := deviceFlag, jsonOutput, ensureFreshCertificateFn
	t.Cleanup(func() { deviceFlag, jsonOutput, ensureFreshCertificateFn = device, asJSON, fresh })
	deviceFlag, jsonOutput = "", false
	ensureFreshCertificateFn = func(context.Context, *config.AuthConfig) error { return nil }
	cmd := newDeviceRenameCmd()
	cmd.SetContext(context.Background())
	return captureCommandStdout(t, func() error { return cmd.RunE(cmd, []string{name}) })
}

const (
	renameDevice = "33333333-3333-4333-8333-333333333333"
	// Distinct from the device UUID: Cloud names the asset, not the agent.
	renameAsset = "44444444-4444-4444-8444-444444444444"
)

const renamePrincipal = "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/device/" + renameDevice

// startV2RenameDevice serves a device enrolled through v2 ACME: the v1
// provisioning answer carries only the cloud host (org and asset 0), and the
// v2 answer carries the PKI principal. An empty principal leaves the v2
// provisioning service unregistered, like an Agent that predates it.
func startV2RenameDevice(t *testing.T, cloud *renameCloudV2, principal string) {
	t.Helper()
	addr := startRenameCloud(t, func(s *grpc.Server) { cloudpbv2.RegisterAssetServiceServer(s, cloud) })
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = addr
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	startUDSAgentServing(t, renameAgent{log: cloud.log}, func(s *grpc.Server) {
		agentpb.RegisterWendyProvisioningServiceServer(s, renameProvisioningV1{prov: &agentpb.ProvisionedResponse{CloudHost: addr}})
		if principal != "" {
			agentpbv2.RegisterWendyProvisioningServiceServer(s, renameProvisioningV2{prov: &agentpbv2.ProvisionedResponse{CloudHost: addr, PrincipalUri: principal}})
		}
	})
}

func TestDeviceRenameV2EnrollmentSignsV2UpdateBeforeHostname(t *testing.T) {
	log := &renameLog{}
	cloud := &renameCloudV2{log: log, asset: &cloudpbv2.Asset{Id: renameAsset, OrganizationId: testOperatorTenant, PkiDeviceName: proto.String(renameDevice)}}
	startV2RenameDevice(t, cloud, renamePrincipal)

	out, err := runDeviceRename(t, "living-room")
	if err != nil {
		t.Fatalf("rename failed: %v\n%s", err, log)
	}
	want := strings.Join([]string{
		"cloud v2 GetAsset org=" + testOperatorTenant + " device=" + renameDevice,
		"cloud v2 UpdateAsset id=" + renameAsset + " name=living-room op=wendycloud.v2.AssetService/UpdateAsset resource=asset/" + renameAsset,
		"agent SetHostname living-room",
	}, "\n")
	if log.String() != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", log, want)
	}
	if !strings.Contains(out, "Renamed asset "+renameAsset+" in Wendy Cloud.") {
		t.Fatalf("output does not name the asset UUID: %s", out)
	}
}

func TestDeviceRenameCloudRefusalLeavesHostnameUnchanged(t *testing.T) {
	for _, refusal := range []error{
		status.Error(codes.AlreadyExists, "another device in this organization is already named living-room"),
		status.Error(codes.InvalidArgument, "device name living-room must start with a lowercase letter"),
	} {
		t.Run(status.Code(refusal).String(), func(t *testing.T) {
			log := &renameLog{}
			cloud := &renameCloudV2{log: log, updateErr: refusal, asset: &cloudpbv2.Asset{Id: renameAsset, OrganizationId: testOperatorTenant, PkiDeviceName: proto.String(renameDevice)}}
			startV2RenameDevice(t, cloud, renamePrincipal)

			_, err := runDeviceRename(t, "living-room")
			if err == nil || !strings.Contains(err.Error(), status.Convert(refusal).Message()) || !strings.Contains(err.Error(), "hostname was not changed") {
				t.Fatalf("refusal not reported: %v", err)
			}
			if strings.Contains(log.String(), "SetHostname") {
				t.Fatalf("hostname changed after Cloud refused the name:\n%s", log)
			}
		})
	}
}

// An enrollment that is not legacy but has no usable v2 identity, or one in
// another organization than the session, is refused before any change: it
// must never fall back to the unsigned v1 UpdateAsset of asset 0.
func TestDeviceRenameUnidentifiedV2EnrollmentChangesNothing(t *testing.T) {
	for _, tc := range []struct{ name, principal, want string }{
		{name: "agent without v2 provisioning", want: "cannot identify"},
		{name: "device in another organization", principal: "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/" + renameDevice, want: "not in the device's organization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &renameLog{}
			startV2RenameDevice(t, &renameCloudV2{log: log}, tc.principal)

			_, err := runDeviceRename(t, "living-room")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if log.String() != "" {
				t.Fatalf("calls made for an unidentified enrollment:\n%s", log)
			}
		})
	}
}

func TestDeviceRenameLegacyEnrollmentKeepsV1Update(t *testing.T) {
	log := &renameLog{}
	addr := startRenameCloud(t, func(s *grpc.Server) { cloudpb.RegisterAssetServiceServer(s, &renameCloudV1{log: log}) })
	auth := fakeAuth(t)
	auth.CloudGRPC = addr
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	startUDSAgentServing(t, renameAgent{log: log}, func(s *grpc.Server) {
		agentpb.RegisterWendyProvisioningServiceServer(s, renameProvisioningV1{prov: &agentpb.ProvisionedResponse{CloudHost: addr, OrganizationId: 7, AssetId: 42}})
	})

	out, err := runDeviceRename(t, "living-room")
	if err != nil {
		t.Fatalf("rename failed: %v\n%s", err, log)
	}
	if want := "cloud v1 UpdateAsset id=42 name=living-room\nagent SetHostname living-room"; log.String() != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", log, want)
	}
	if !strings.Contains(out, "Renamed asset 42 in Wendy Cloud.") {
		t.Fatalf("output does not name the legacy asset: %s", out)
	}
}

func TestValidateHostnameArg(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"simple", "wendy", false},
		{"with prefix", "wendyos-living-room", false},
		{"single letter", "a", false},
		{"digits and hyphens", "node-1-of-3", false},
		{"max length 63", strings.Repeat("a", 63), false},

		{"empty", "", true},
		{"too long 64", strings.Repeat("a", 64), true},
		{"starts with digit", "1wendy", true},
		{"starts with hyphen", "-wendy", true},
		{"ends with hyphen", "wendy-", true},
		{"uppercase", "Wendy", true},
		{"underscore", "wendy_os", true},
		{"dot", "wendy.os", true},
		{"space", "wendy os", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHostnameArg(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateHostnameArg(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestResolveRenameNameFromArg(t *testing.T) {
	got, err := resolveRenameName([]string{"  living-room  "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "living-room" {
		t.Errorf("resolveRenameName trimmed = %q, want %q", got, "living-room")
	}

	if _, err := resolveRenameName([]string{"Bad_Name"}); err == nil {
		t.Errorf("resolveRenameName with invalid arg: expected error, got nil")
	}
}

func TestRepointDefaultDeviceFollowsARenamedMDNSDefault(t *testing.T) {
	origFlag := deviceFlag
	t.Cleanup(func() { deviceFlag = origFlag })
	deviceFlag = ""

	setTempConfig(t, &config.Config{
		DefaultDevice: "old-name.local",
		DevicePins:    map[string]config.DevicePin{"old-name": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"}},
	})
	repointDefaultDevice("new-name")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultDevice != "new-name.local" {
		t.Fatalf("DefaultDevice = %q, want new-name.local", cfg.DefaultDevice)
	}
	if _, ok := cfg.DevicePinFor("old-name"); !ok {
		t.Fatal("repointing the default dropped an unrelated field")
	}

	setTempConfig(t, &config.Config{DefaultDevice: "192.168.1.5"})
	repointDefaultDevice("new-name")
	if cfg, _ := config.Load(); cfg.DefaultDevice != "192.168.1.5" {
		t.Fatalf("an IP default was repointed to %q", cfg.DefaultDevice)
	}
}
