package commands

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	legacyagentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestBuildDeliveryTargetMatchesAuthenticatedDevice(t *testing.T) {
	actual := deploymentV2Device().PrincipalUri
	id := actual[strings.LastIndex(actual, "/")+1:]
	otherID := "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct {
		name, reportedPrincipal, reportedUUID, operator string
		plaintext, wantError                            bool
	}{
		{name: "TLS-backed direct or tunnel connection", reportedPrincipal: actual, reportedUUID: id},
		{name: "another target UUID", reportedPrincipal: actual, reportedUUID: otherID, wantError: true},
		{name: "another device", reportedPrincipal: strings.Replace(actual, id, otherID, 1), reportedUUID: otherID, wantError: true},
		{name: "another tenant", reportedPrincipal: strings.Replace(actual, deploymentTenant, otherID, 1), reportedUUID: id, wantError: true},
		{name: "another operator tenant", reportedPrincipal: actual, reportedUUID: id, operator: otherID, wantError: true},
		{name: "plaintext claimed UUID", reportedPrincipal: actual, reportedUUID: id, plaintext: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device := deploymentV2Device()
			device.PrincipalUri = tc.reportedPrincipal
			device.AssetUuid = tc.reportedUUID
			server := &deploymentV2Server{device: device}
			conn := deploymentTestConnection(t, server, nil)
			if !tc.plaintext {
				conn = deploymentTLSConnection(t, server, nil, actual)
			}
			conn.Addr = "127.0.0.1:50052"
			version := &fakeAgentVersionClient{resp: &legacyagentpb.GetAgentVersionResponse{Os: "linux"}}
			conn.AgentService = version
			if tc.operator != "" {
				conn.CertInfo = &config.CertificateInfo{PrincipalURI: "spiffe://wendy.sh/tenant/" + tc.operator + "/operator/test"}
			}
			target, err := targetPushTargetBase(context.Background(), conn)
			if (err != nil) != tc.wantError {
				t.Fatalf("target identity binding: target=%v, error=%v, wantError=%v", target, err, tc.wantError)
			}
			if tc.wantError && version.calls != 0 {
				t.Fatalf("unverified target reached subsequent device operations: %d", version.calls)
			}
			if !tc.wantError && target.GetAssetUuid() != id {
				t.Fatalf("delivery target changed the proved device: %+v", target)
			}
		})
	}
}

func TestBuildDeliveryTargetUsesVerifiedSessionBrokerIdentity(t *testing.T) {
	device := deploymentV2Device()
	device.AssetUuid = device.PrincipalUri[strings.LastIndex(device.PrincipalUri, "/")+1:]
	identity, err := certs.ParsePrincipal(device.PrincipalUri)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		identity  certs.WendyIdentity
		wantError bool
	}{
		{name: "matching retained principal", identity: identity},
		{name: "another retained device", identity: certs.WendyIdentity{Principal: strings.Replace(device.PrincipalUri, device.AssetUuid, "11111111-1111-4111-8111-111111111111", 1)}, wantError: true},
		{name: "numeric broker cannot prove a UUID target", identity: certs.WendyIdentity{OrgID: 42, EntityType: certs.EntityAsset, EntityID: "9"}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := grpcclient.ConnectSessionProxy(context.Background(), filepath.Join(t.TempDir(), "unused.sock"), "target", "target:50052", nil, tc.identity)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			proved, err := verifiedBuildDeliveryDevice(conn, nil, device)
			if (err != nil) != tc.wantError {
				t.Fatalf("broker delivery binding: device=%v error=%v wantError=%v", proved, err, tc.wantError)
			}
		})
	}
}
