package commands

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type infoProvisioningV2Server struct {
	agentpbv2.UnimplementedWendyProvisioningServiceServer
	response *agentpbv2.IsProvisionedResponse
	err      error
}

func (s infoProvisioningV2Server) IsProvisioned(context.Context, *agentpbv2.IsProvisionedRequest) (*agentpbv2.IsProvisionedResponse, error) {
	return s.response, s.err
}

type infoProvisioningV1Server struct {
	agentpb.UnimplementedWendyProvisioningServiceServer
	response *agentpb.IsProvisionedResponse
}

func (s infoProvisioningV1Server) IsProvisioned(context.Context, *agentpb.IsProvisionedRequest) (*agentpb.IsProvisionedResponse, error) {
	return s.response, nil
}

func TestDeviceInfoOrganization(t *testing.T) {
	const endpoint = "device-cloud:443"
	v2 := func(id int32, principal string) *agentpbv2.IsProvisionedResponse {
		return &agentpbv2.IsProvisionedResponse{ResponseType: &agentpbv2.IsProvisionedResponse_Provisioned{
			Provisioned: &agentpbv2.ProvisionedResponse{CloudHost: endpoint, OrganizationId: id, PrincipalUri: principal},
		}}
	}
	cases := []struct {
		name      string
		v2        *agentpbv2.IsProvisionedResponse
		v1        *agentpb.IsProvisionedResponse
		err       error
		cached    string
		wantID    string
		wantLabel string
		wantNull  bool
	}{
		{
			name:   "tenant UUID takes precedence over legacy ID",
			v2:     v2(42, "spiffe://wendy.sh/tenant/"+testOperatorTenant+"/device/test"),
			cached: "Robotics", wantID: testOperatorTenant, wantLabel: "Robotics (" + testOperatorTenant + ")",
		},
		{name: "numeric v2 enrollment", v2: v2(42, ""), wantID: "42", wantLabel: "42"},
		{
			name: "legacy agent fallback",
			v1: &agentpb.IsProvisionedResponse{Response: &agentpb.IsProvisionedResponse_Provisioned{
				Provisioned: &agentpb.ProvisionedResponse{CloudHost: endpoint, OrganizationId: 42},
			}},
			cached: "Legacy Robotics", wantID: "42", wantLabel: "Legacy Robotics (42)",
		},
		{
			name: "unenrolled v2",
			v2: &agentpbv2.IsProvisionedResponse{ResponseType: &agentpbv2.IsProvisionedResponse_NotProvisioned{
				NotProvisioned: &agentpbv2.NotProvisionedResponse{},
			}},
			wantNull: true, wantLabel: "Not enrolled",
		},
		{
			name: "unenrolled legacy",
			v1: &agentpb.IsProvisionedResponse{Response: &agentpb.IsProvisionedResponse_NotProvisioned{
				NotProvisioned: &agentpb.NotProvisionedResponse{},
			}},
			wantNull: true, wantLabel: "Not enrolled",
		},
		{name: "unsupported agent"},
		{name: "unavailable enrollment", err: status.Error(codes.Unavailable, "offline")},
		{name: "missing enrollment response", v2: &agentpbv2.IsProvisionedResponse{}},
		{name: "enrolled without organization", v2: v2(0, "")},
		{name: "invalid tenant does not use legacy ID", v2: v2(42, "invalid")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// An unrelated default and a same-ID organization on another cloud
			// must never supply the device's organization name.
			seedConfig(t, &config.Config{DefaultCloudGRPC: "other-cloud:443", DefaultOrgID: 7})
			cacheCloudOrganizationName("other-cloud:443", "42", "Wrong cloud")
			cacheCloudOrganizationName(endpoint, "7", "Wrong organization")
			if tc.cached != "" {
				cacheCloudOrganizationName(endpoint, tc.wantID, tc.cached)
			}
			startUDSAgent(t, func(srv *grpc.Server) {
				if tc.v2 != nil || tc.err != nil {
					agentpbv2.RegisterWendyProvisioningServiceServer(srv, infoProvisioningV2Server{response: tc.v2, err: tc.err})
				}
				if tc.v1 != nil {
					agentpb.RegisterWendyProvisioningServiceServer(srv, infoProvisioningV1Server{response: tc.v1})
				}
			})
			oldJSON := jsonOutput
			t.Cleanup(func() { jsonOutput = oldJSON })
			for _, asJSON := range []bool{false, true} {
				jsonOutput = asJSON
				cmd := newDeviceInfoCmd()
				cmd.SetContext(context.Background())
				out, err := captureCommandStdout(t, func() error { return cmd.RunE(cmd, nil) })
				if err != nil {
					t.Fatalf("device info failed: %v", err)
				}
				if !asJSON {
					out = ansi.Strip(out)
					if tc.wantLabel != "" {
						if !strings.Contains(out, "Organization: "+tc.wantLabel+"\n") {
							t.Fatalf("missing organization label in %s", out)
						}
					} else if strings.Contains(out, "Organization:") {
						t.Fatalf("reported unknown enrollment as known: %s", out)
					}
					continue
				}
				var data map[string]json.RawMessage
				if err := json.Unmarshal([]byte(out), &data); err != nil {
					t.Fatalf("invalid JSON: %v: %s", err, out)
				}
				org, present := data["organization"]
				switch {
				case tc.wantNull:
					if string(org) != "null" {
						t.Fatalf("unenrolled organization = %s, want null", org)
					}
				case tc.wantID != "":
					var fields map[string]string
					if err := json.Unmarshal(org, &fields); err != nil {
						t.Fatal(err)
					}
					if fields["id"] != tc.wantID || fields["name"] != tc.cached {
						t.Fatalf("organization = %s", org)
					}
					if _, hasName := fields["name"]; tc.cached == "" && hasName {
						t.Fatalf("empty name should be omitted: %s", org)
					}
				default:
					if present {
						t.Fatalf("unknown enrollment should be omitted: %s", org)
					}
				}
			}
		})
	}
}

func TestDeviceOrganizationNameUsesEnrollment(t *testing.T) {
	const endpoint = "device-cloud:443"
	seedConfig(t, &config.Config{
		DefaultCloudGRPC: endpoint, DefaultOrgID: 7,
		Auth: []config.AuthConfig{
			{CloudGRPC: "other-cloud:443", Certificates: []config.CertificateInfo{{OrganizationID: 42}}},
			{CloudGRPC: endpoint, Certificates: []config.CertificateInfo{{OrganizationID: 7}, {OrganizationID: 42}}},
		},
	})
	old := listOrgsFromCloud
	t.Cleanup(func() { listOrgsFromCloud = old })
	var lookupErr error
	listOrgsFromCloud = func(ctx context.Context, auth *config.AuthConfig) ([]*cloudpb.Organization, error) {
		if auth.CloudGRPC != endpoint || auth.OrganizationKey() != "42" {
			t.Fatalf("lookup used wrong cloud or organization: %s %s", auth.CloudGRPC, auth.OrganizationKey())
		}
		return []*cloudpb.Organization{{Id: 7, Name: "Default"}, {Id: 42, Name: "Device organization"}}, lookupErr
	}
	if got := deviceOrganizationName(context.Background(), endpoint, "42"); got != "Device organization" {
		t.Fatalf("name = %q", got)
	}
	lookupErr = errors.New("offline")
	if got := deviceOrganizationName(context.Background(), endpoint, "42"); got != "Device organization" {
		t.Fatalf("offline name = %q", got)
	}
}
