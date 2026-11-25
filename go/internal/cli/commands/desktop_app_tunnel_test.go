package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

const (
	appTunnelTestTenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	appTunnelTestAlice  = "spiffe://wendy.sh/tenant/" + appTunnelTestTenant + "/operator/alice"
	appTunnelTestBob    = "spiffe://wendy.sh/tenant/" + appTunnelTestTenant + "/operator/bob"
)

func TestDesktopAppTunnelConnectorPreservesOperatorAndDevice(t *testing.T) {
	t.Setenv("WENDY_AGENT_SOCKET", "")
	const asset = "7791d76e-a942-4a8e-b582-063d063b5623"
	const endpoint = "cloud.example:443"
	selector := cloudDeviceSelector{Endpoint: endpoint, TenantUUID: appTunnelTestTenant, AssetUUID: asset}.String()
	originalLeaf := localUnenrollOperatorCert(t, appTunnelTestAlice)
	observer := &grpcclient.VerifiedServerIdentity{}
	identity, err := certs.ParsePrincipal(certs.DeviceSPIFFEURI(appTunnelTestTenant, "thermal-camera"))
	if err != nil {
		t.Fatal(err)
	}
	observer.Observe(identity)
	original := grpcclient.NewFromConnWithVerifiedIdentity(nil, observer)
	original.CertInfo = originalLeaf
	reconnect := DesktopAppTunnelConnector(original)
	oldFetch, oldConnect := fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn
	t.Cleanup(func() { fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn = oldFetch, oldConnect })

	for _, tc := range []struct {
		name, operator, device, wantError string
		wantFetch, wantConnect            bool
	}{
		{name: "new key for the same operator", operator: appTunnelTestAlice, device: "thermal-camera", wantFetch: true, wantConnect: true},
		{name: "different operator in the same tenant", operator: appTunnelTestBob, device: "thermal-camera", wantError: "operator changed"},
		{name: "asset rebound to another device", operator: appTunnelTestAlice, device: "replacement-camera", wantError: "device identity changed", wantFetch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := localUnenrollOperatorCert(t, tc.operator)
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{{CloudGRPC: endpoint, Certificates: []config.CertificateInfo{*saved}}}})
			discovered, connected := false, false
			fetchDefaultCloudAssetsV2Fn = func(context.Context, *config.AuthConfig, bool) ([]*cloudpbv2.Asset, error) {
				discovered = true
				return []*cloudpbv2.Asset{{Id: asset, PkiDeviceName: &tc.device}}, nil
			}
			connectDefaultCloudAssetV2Fn = func(_ context.Context, auth *config.AuthConfig, _ *cloudpbv2.Asset, _ string) (*grpcclient.AgentConnection, error) {
				connected = true
				if auth.Certificates[0].PemCertificate != saved.PemCertificate {
					t.Error("did not use the replacement certificate")
				}
				return &grpcclient.AgentConnection{}, nil
			}
			conn, err := reconnect(t.Context(), selector)
			defer conn.Close()
			if tc.wantError == "" {
				if err != nil || conn == nil {
					t.Fatalf("same operator reconnect returned %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) || conn != nil {
				t.Fatalf("reconnect returned %v, want %q", err, tc.wantError)
			}
			if discovered != tc.wantFetch || connected != tc.wantConnect {
				t.Fatalf("discovery = %t, connected = %t", discovered, connected)
			}
		})
	}
}

func TestAppTunnelOperatorPinSurvivesAuthorizationReload(t *testing.T) {
	original := config.AuthConfig{
		CloudGRPC: "cloud.example:443", OAuthIssuer: "https://auth.example",
		Certificates: []config.CertificateInfo{*localUnenrollOperatorCert(t, appTunnelTestAlice)},
	}
	replacement := original
	replacement.Certificates = []config.CertificateInfo{*localUnenrollOperatorCert(t, appTunnelTestBob)}
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{replacement}})

	scope := context.WithValue(t.Context(), appTunnelIdentityKey{}, appTunnelIdentity{operator: appTunnelTestAlice})
	for _, tc := range []struct {
		name    string
		prepare func(context.Context, *config.AuthConfig) error
	}{
		{name: "certificate setup", prepare: prepareDefaultCloudCertificate},
		{name: "tunnel grant token", prepare: ensureOAuthAccessToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := original
			if err := tc.prepare(carryTunnelSession(t.Context(), scope), &snapshot); err == nil || !strings.Contains(err.Error(), "operator changed") {
				t.Fatalf("auth reload accepted a different operator: %v", err)
			}
		})
	}
}
