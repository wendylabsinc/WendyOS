package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func TestCloudSelectorResolvesExactCloudOrgAndAsset(t *testing.T) {
	auth7, auth9 := pickerAuth(7), pickerAuth(9)
	otherCloud := *auth7
	otherCloud.CloudGRPC = "other.example:443"
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth9, otherCloud, *auth7}, DefaultOrgID: 9})
	oldFetch, oldConnect := fetchDefaultCloudAssetsFn, connectDefaultCloudAssetFn
	t.Cleanup(func() { fetchDefaultCloudAssetsFn, connectDefaultCloudAssetFn = oldFetch, oldConnect })
	fetchDefaultCloudAssetsFn = func(_ context.Context, auth *config.AuthConfig) ([]*cloudpb.Asset, error) {
		if auth.CloudGRPC != auth7.CloudGRPC || cloudAuthOrgID(auth) != 7 {
			t.Fatal("used the default org instead of the saved org")
		}
		return []*cloudpb.Asset{{Id: 99, Name: "42"}, {Id: 42, Name: "renamed"}}, nil
	}
	connectDefaultCloudAssetFn = func(_ context.Context, auth *config.AuthConfig, asset *cloudpb.Asset, _ string) (*grpcclient.AgentConnection, error) {
		if asset.Id != 42 || cloudAuthOrgID(auth) != 7 {
			t.Fatal("connected to wrong asset or organization")
		}
		return &grpcclient.AgentConnection{Host: asset.Name}, nil
	}
	selector := cloudDeviceSelector{Endpoint: auth7.CloudGRPC, OrgID: 7, AssetID: 42}
	selected, err := connectCloudDeviceSelector(context.Background(), selector)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Selector != selector.String() {
		t.Fatalf("kept tunnel/display address: %q", selected.Selector)
	}
	selector.AssetID = 100
	if _, err := connectCloudDeviceSelector(context.Background(), selector); err == nil {
		t.Fatal("missing asset silently selected another")
	}
	selector.OrgID = 100
	if _, err := connectCloudDeviceSelector(context.Background(), selector); err == nil {
		t.Fatal("missing organization silently selected another")
	}
}

func TestCloudSelectorsReachBothConnectionPathsAndMCP(t *testing.T) {
	key := "cloud://cloud.example:443/org/7/asset/42"
	setTempConfig(t, &config.Config{})
	t.Setenv("WENDY_AGENT_SOCKET", "")
	oldFlag, oldJSON, oldConnect := deviceFlag, jsonOutput, connectCloudDeviceSelectorFn
	t.Cleanup(func() { deviceFlag, jsonOutput, connectCloudDeviceSelectorFn = oldFlag, oldJSON, oldConnect })
	deviceFlag, jsonOutput = key, true
	calls := 0
	connectCloudDeviceSelectorFn = func(_ context.Context, selector cloudDeviceSelector) (*SelectedDevice, error) {
		calls++
		if selector.AssetID != 42 {
			t.Fatal("wrong asset")
		}
		return &SelectedDevice{Agent: &grpcclient.AgentConnection{Host: "Go2"}}, nil
	}
	if _, err := connectToAgent(context.Background(), NonInteractive()); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveTargetInner(context.Background(), NonInteractive()); err != nil {
		t.Fatal(err)
	}
	if _, err := connectMCPDevice(context.Background(), mcpStartupAddress(key)); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("routed %d calls", calls)
	}
	// A failed named target never falls through to LAN discovery or another
	// organization.
	failure := errors.New("selected asset offline")
	connectCloudDeviceSelectorFn = func(_ context.Context, selector cloudDeviceSelector) (*SelectedDevice, error) {
		if selector.AssetID != 43 {
			t.Fatal("wrong asset")
		}
		return nil, failure
	}
	deviceFlag = "cloud://cloud.example:443/org/7/asset/43"
	if _, err := connectToAgent(context.Background(), NonInteractive()); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err := resolveTargetInner(context.Background(), NonInteractive()); !errors.Is(err, failure) {
		t.Fatal(err)
	}
}

// I1 fix: the MCP connect path (wendy mcp serve --device 283 /
// WENDY_DEVICE=283 wendy mcp serve, and the device_connect tool) reaches the
// agent through connectMCPDevice, not connectToAgent/resolveTarget, so it
// needs its own numeric-name guard before the direct TLS dial.
func TestConnectMCPDeviceRejectsNumericDeviceBeforeDialling(t *testing.T) {
	setTempConfig(t, &config.Config{})
	t.Setenv("WENDY_AGENT_SOCKET", "")
	origLadder := dialAgentLadderFn
	dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
		t.Error("dialled a numeric device name; it must be rejected before any connection attempt")
		return nil, nil, errors.New("unreachable in test")
	}
	t.Cleanup(func() { dialAgentLadderFn = origLadder })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, bad := range []string{"283", "283:50051"} {
		if _, err := connectMCPDevice(ctx, bad); !errors.Is(err, errInvalidDeviceName) {
			t.Errorf("connectMCPDevice(ctx, %q) err = %v, want errInvalidDeviceName", bad, err)
		}
	}
}

// The cloud path treats a bare number as an asset ID; connectMCPDevice's
// numeric guard must not intercept an actual cloud selector. It is fine for
// this to fail for another reason (not logged in, here).
func TestConnectMCPDeviceLeavesCloudSelectorToTheCloudPath(t *testing.T) {
	setTempConfig(t, &config.Config{})
	t.Setenv("WENDY_AGENT_SOCKET", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := connectMCPDevice(ctx, "cloud://grpc.a.sh:443/org/7/asset/283"); errors.Is(err, errInvalidDeviceName) {
		t.Fatalf("the cloud selector was rejected as a numeric device name: %v", err)
	}
}

func TestCloudDeviceSelectorValidationAndMCPAddress(t *testing.T) {
	for _, value := range []string{"cloud://host/org/0/asset/1", "cloud://host/org/7/asset/-1", "cloud://host/org/7/asset/2147483648",
		"cloud://user:pass@host/org/1/asset/2", "cloud://host/org/1/asset/2?org=3", "cloud:42", "cloud://host/org/1/asset/2#extra"} {
		if _, matched, err := parseCloudDeviceSelector(value); !matched || err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	for _, value := range []string{"cloud://host:443/org/1/asset/2", "vm:dev", "sim"} {
		if mcpStartupAddress(value) != value {
			t.Fatalf("MCP corrupted %q", value)
		}
	}
	if mcpStartupAddress("robot.local") != "robot.local:50051" {
		t.Fatal("LAN address changed")
	}
}

// MCP device_connect dials what the startup device dials. A bare mTLS host
// failed ("no usable client certificate") while host:50051 connects, and
// the run tool replays the target as host:50051.
func TestMCPDeviceConnectDialsBareHostsAsHostPort(t *testing.T) {
	t.Setenv("WENDY_AGENT_SOCKET", "")
	old := connectMCPDirectFn
	t.Cleanup(func() { connectMCPDirectFn = old })
	for device, want := range map[string]string{
		"robot.local":       "robot.local:50051",
		"fe80::1":           "[fe80::1]:50051",
		"robot.local:50052": "robot.local:50052",
		// install verify passes a USB link-local address with its zone; the
		// dial must keep it (WDY-3974).
		"fe80::1%en31":         "[fe80::1%en31]:50051",
		"[fe80::1%en31]:50051": "[fe80::1%en31]:50051",
	} {
		var dialed string
		connectMCPDirectFn = func(_ context.Context, address string) (*grpcclient.AgentConnection, error) {
			dialed = address
			return &grpcclient.AgentConnection{Addr: address}, nil
		}
		if _, err := connectMCPDevice(context.Background(), device); err != nil || dialed != want {
			t.Errorf("connectMCPDevice(%q) dialed %q (err %v), want %q", device, dialed, err, want)
		}
	}
}

func TestCloudV2SelectorPreservesTenantAndAssetAcrossRename(t *testing.T) {
	const tenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	const assetID = "7791d76e-a942-4a8e-b582-063d063b5623"
	auth := &config.AuthConfig{CloudGRPC: "cloud.example:443", Certificates: []config.CertificateInfo{{
		PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test",
	}}}
	otherTenant := *auth
	otherTenant.Certificates = []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/ed6e09f3-d287-450b-a053-e4554f3c70ed/operator/test"}}
	otherCloud := *auth
	otherCloud.CloudGRPC = "other.example:443"
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{otherTenant, otherCloud, *auth}})
	asset := &cloudpbv2.Asset{Id: assetID, Name: "original"}
	want := "cloud://cloud.example:443/tenant/" + tenant + "/asset/" + assetID
	if got, err := cloudDiscoverySelector(auth, cloudDiscoveryDevice{cloudAssetMetadata: asset, v2: asset, key: assetID}); err != nil || got != want {
		t.Fatalf("UUID selector = %q, %v; want %q", got, err, want)
	}
	selector, matched, err := parseCloudDeviceSelector(want)
	if err != nil || !matched || selector.String() != want {
		t.Fatalf("selector round trip: %+v, %v", selector, err)
	}
	oldFetch, oldConnect := fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn
	t.Cleanup(func() { fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn = oldFetch, oldConnect })
	fetchDefaultCloudAssetsV2Fn = func(_ context.Context, selected *config.AuthConfig, onlineOnly bool) ([]*cloudpbv2.Asset, error) {
		if selected.CloudGRPC != auth.CloudGRPC || selected.Certificates[0].TenantUUID() != tenant || !onlineOnly {
			t.Fatal("selector resolved the wrong cloud or tenant")
		}
		return []*cloudpbv2.Asset{{Id: "a20403ae-9251-4937-aa9b-bb1d773df071", Name: "original"}, {Id: assetID, Name: "renamed"}}, nil
	}
	connectDefaultCloudAssetV2Fn = func(_ context.Context, _ *config.AuthConfig, selected *cloudpbv2.Asset, _ string) (*grpcclient.AgentConnection, error) {
		if selected.Id != assetID || selected.Name != "renamed" {
			t.Fatal("selector resolved by display name instead of UUID")
		}
		return &grpcclient.AgentConnection{Host: selected.Name}, nil
	}
	selected, err := connectCloudDeviceSelector(context.Background(), selector)
	if err != nil || selected.Selector != want {
		t.Fatalf("connect selector: %v, %v", selected, err)
	}
	selector.AssetUUID = "ede01a43-7ef8-49d9-bfaf-88192b5124af"
	if _, err := connectCloudDeviceSelector(context.Background(), selector); err == nil {
		t.Fatal("missing asset selected another device")
	}
	selector.TenantUUID = "4d344658-f8f6-4c1e-aa14-9d96c2d56963"
	if _, err := selector.auth(&config.Config{Auth: []config.AuthConfig{*auth}}); err == nil {
		t.Fatal("missing tenant selected another login")
	}
	for _, key := range []string{
		"cloud://cloud.example:443/tenant/not-a-uuid/asset/" + assetID,
		"cloud://cloud.example:443/tenant/" + tenant + "/asset/00000000-0000-0000-0000-000000000000",
	} {
		if _, matched, err := parseCloudDeviceSelector(key); !matched || err == nil {
			t.Fatalf("invalid UUID selector accepted: %s", key)
		}
	}
}

func TestCloudSelectorRenewsSelectedCertificateBeforeDiscovery(t *testing.T) {
	const tenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	const assetID = "7791d76e-a942-4a8e-b582-063d063b5623"
	auth := config.AuthConfig{CloudGRPC: "cloud.example:443", Certificates: []config.CertificateInfo{{
		PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test", PemCertificate: "expiring",
	}}}
	other := auth
	other.Certificates = []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/ed6e09f3-d287-450b-a053-e4554f3c70ed/operator/test", PemCertificate: "other"}}
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{other, auth}})
	t.Setenv(renewEndpointEnv, "https://renew.example/v1/renew")
	oldExpiry, oldRenew := leafNotAfterFn, renewViaPKICore
	oldFetch, oldConnect := fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn
	t.Cleanup(func() {
		leafNotAfterFn, renewViaPKICore = oldExpiry, oldRenew
		fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn = oldFetch, oldConnect
	})
	leafNotAfterFn = func(string) (time.Time, error) { return time.Now().Add(time.Minute), nil }
	renewed := false
	renewViaPKICore = func(_ context.Context, _ string, selected *config.AuthConfig) (string, string, string, error) {
		if selected.Certificates[0].TenantUUID() != tenant {
			t.Fatal("renewed a different tenant's certificate")
		}
		renewed = true
		return "renewed", "chain", "key", nil
	}
	fetchDefaultCloudAssetsV2Fn = func(_ context.Context, selected *config.AuthConfig, _ bool) ([]*cloudpbv2.Asset, error) {
		if !renewed || selected.Certificates[0].PemCertificate != "renewed" {
			t.Fatal("discovered assets before renewing the selected certificate")
		}
		return []*cloudpbv2.Asset{{Id: assetID}}, nil
	}
	connectDefaultCloudAssetV2Fn = func(_ context.Context, selected *config.AuthConfig, _ *cloudpbv2.Asset, _ string) (*grpcclient.AgentConnection, error) {
		if selected.Certificates[0].PemCertificate != "renewed" {
			t.Fatal("opened tunnel with the old certificate")
		}
		return &grpcclient.AgentConnection{}, nil
	}
	if _, err := connectCloudDeviceSelector(context.Background(), cloudDeviceSelector{Endpoint: auth.CloudGRPC, TenantUUID: tenant, AssetUUID: assetID}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil || cfg.Auth[0].Certificates[0].PemCertificate != "other" || cfg.Auth[1].Certificates[0].PemCertificate != "renewed" {
		t.Fatalf("renewal changed the wrong stored session: %v", err)
	}
}

func TestCloudSelectorRefreshesExpiredOAuthCertificate(t *testing.T) {
	const tenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	const assetID = "7791d76e-a942-4a8e-b582-063d063b5623"
	refused := errors.New("identity issuance refused")
	for _, refreshErr := range []error{nil, refused} {
		t.Run(fmt.Sprint(refreshErr), func(t *testing.T) {
			auth := config.AuthConfig{CloudGRPC: "cloud.example:443", OAuthIssuer: "https://auth.example", RefreshToken: "original", Certificates: []config.CertificateInfo{{
				PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test", PemCertificate: "expired",
			}}}
			other := auth
			other.Certificates = []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/ed6e09f3-d287-450b-a053-e4554f3c70ed/operator/test", PemCertificate: "other"}}
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{other, auth}})
			oldExpiry, oldRefresh := leafNotAfterFn, refreshDefaultCloudCertificateFn
			oldFetch, oldConnect := fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn
			t.Cleanup(func() {
				leafNotAfterFn, refreshDefaultCloudCertificateFn = oldExpiry, oldRefresh
				fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn = oldFetch, oldConnect
			})
			leafNotAfterFn = func(string) (time.Time, error) { return time.Now().Add(-time.Hour), nil }
			refreshDefaultCloudCertificateFn = func(_ context.Context, selected *config.AuthConfig) error {
				if selected.Certificates[0].TenantUUID() != tenant {
					t.Fatal("refreshed a different tenant's session")
				}
				selected.RefreshToken = "rotated"
				if refreshErr == nil {
					selected.Certificates[0].PemCertificate = "fresh"
				}
				return refreshErr
			}
			discovered, connected := false, false
			fetchDefaultCloudAssetsV2Fn = func(_ context.Context, selected *config.AuthConfig, _ bool) ([]*cloudpbv2.Asset, error) {
				discovered = true
				if selected.Certificates[0].PemCertificate != "fresh" {
					t.Fatal("discovered assets with an expired certificate")
				}
				return []*cloudpbv2.Asset{{Id: assetID}}, nil
			}
			connectDefaultCloudAssetV2Fn = func(_ context.Context, selected *config.AuthConfig, _ *cloudpbv2.Asset, _ string) (*grpcclient.AgentConnection, error) {
				connected = true
				if selected.Certificates[0].PemCertificate != "fresh" {
					t.Fatal("opened tunnel with an expired certificate")
				}
				return &grpcclient.AgentConnection{}, nil
			}
			_, err := connectCloudDeviceSelector(context.Background(), cloudDeviceSelector{Endpoint: auth.CloudGRPC, TenantUUID: tenant, AssetUUID: assetID})
			if refreshErr == nil {
				if err != nil || !discovered || !connected {
					t.Fatalf("fresh session did not reach the device: %v", err)
				}
			} else if !errors.Is(err, refused) || discovered || connected {
				t.Fatalf("refresh failure was obscured by discovery/tunnel: %v, discovered=%v, connected=%v", err, discovered, connected)
			}
			cfg, err := config.Load()
			if err != nil || cfg.Auth[0].RefreshToken != "original" || cfg.Auth[1].RefreshToken != "rotated" {
				t.Fatalf("rotated token was lost or persisted to the wrong session: %v", err)
			}
		})
	}
}

func TestCloudSelectorKeepsValidCertificateWhenRenewalRefused(t *testing.T) {
	for _, refusal := range []renewUnavailableError{
		{reason: "renewal service unavailable"},
		{reason: "renewal budget exhausted", needsFreshCert: true},
	} {
		t.Run(refusal.reason, func(t *testing.T) {
			auth := pickerAuth(7)
			auth.OAuthIssuer = "https://auth.example"
			auth.Certificates[0].PemCertificate = "still-valid"
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
			t.Setenv(renewEndpointEnv, "https://renew.example/v1/renew")
			oldExpiry, oldRenew := leafNotAfterFn, renewViaPKICore
			oldFetch, oldConnect := fetchDefaultCloudAssetsFn, connectDefaultCloudAssetFn
			oldRefresh := refreshDefaultCloudCertificateFn
			t.Cleanup(func() {
				leafNotAfterFn, renewViaPKICore = oldExpiry, oldRenew
				fetchDefaultCloudAssetsFn, connectDefaultCloudAssetFn = oldFetch, oldConnect
				refreshDefaultCloudCertificateFn = oldRefresh
			})
			leafNotAfterFn = func(string) (time.Time, error) { return time.Now().Add(time.Minute), nil }
			renewViaPKICore = func(context.Context, string, *config.AuthConfig) (string, string, string, error) {
				return "", "", "", refusal
			}
			refreshDefaultCloudCertificateFn = func(context.Context, *config.AuthConfig) error {
				t.Fatal("reissued a certificate that remains valid")
				return nil
			}
			fetchDefaultCloudAssetsFn = func(context.Context, *config.AuthConfig) ([]*cloudpb.Asset, error) {
				return []*cloudpb.Asset{{Id: 42}}, nil
			}
			connected := false
			connectDefaultCloudAssetFn = func(_ context.Context, selected *config.AuthConfig, _ *cloudpb.Asset, _ string) (*grpcclient.AgentConnection, error) {
				connected = true
				if selected.Certificates[0].PemCertificate != "still-valid" {
					t.Fatal("discarded the valid certificate after a renewal refusal")
				}
				return &grpcclient.AgentConnection{}, nil
			}
			warning := captureStderr(t, func() {
				if _, err := connectCloudDeviceSelector(context.Background(), cloudDeviceSelector{Endpoint: auth.CloudGRPC, OrgID: 7, AssetID: 42}); err != nil {
					t.Fatal(err)
				}
			})
			if !connected || !strings.Contains(strings.ToLower(warning), refusal.reason) {
				t.Fatalf("renewal refusal did not warn and continue: connected=%v, warning=%q", connected, warning)
			}
		})
	}
}

// Cloud selectors and device pins select sessions with the same organization
// rule: a numeric organization belongs to legacy sessions on its endpoint.
func TestCloudSelectorUsesPinOrganizationMatching(t *testing.T) {
	const tenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	legacy := config.AuthConfig{CloudGRPC: "cloud.example:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}}
	tenantSession := config.AuthConfig{CloudGRPC: legacy.CloudGRPC, Certificates: []config.CertificateInfo{{
		OrganizationID: 7, PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test",
	}}}
	selector := cloudDeviceSelector{Endpoint: legacy.CloudGRPC, OrgID: 7, AssetID: 42}
	if _, err := selector.auth(&config.Config{Auth: []config.AuthConfig{tenantSession}}); err == nil {
		t.Fatal("tenant session served a legacy organization selector")
	}
	if auth, err := selector.auth(&config.Config{Auth: []config.AuthConfig{tenantSession, legacy}}); err != nil || auth.Certificates[0].TenantUUID() != "" {
		t.Fatalf("selector session = %+v, %v; want the legacy session", auth, err)
	}
}

// Renewal can reissue the selected session in another organization. The
// selector re-checks it before discovery, as device pins do.
func TestCloudSelectorRechecksSessionAfterRenewal(t *testing.T) {
	const tenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	auth := config.AuthConfig{CloudGRPC: "cloud.example:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}}
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{auth}})
	oldFresh, oldFetch := ensureFreshCertificateFn, fetchDefaultCloudAssetsFn
	t.Cleanup(func() { ensureFreshCertificateFn, fetchDefaultCloudAssetsFn = oldFresh, oldFetch })
	ensureFreshCertificateFn = func(_ context.Context, selected *config.AuthConfig) error {
		selected.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/" + tenant + "/operator/test"
		return nil
	}
	fetchDefaultCloudAssetsFn = func(context.Context, *config.AuthConfig) ([]*cloudpb.Asset, error) {
		t.Fatal("discovered assets with a session outside the saved organization")
		return nil, nil
	}
	_, err := connectCloudDeviceSelector(context.Background(), cloudDeviceSelector{Endpoint: auth.CloudGRPC, OrgID: 7, AssetID: 42})
	if err == nil || !strings.Contains(err.Error(), "no login for cloud") {
		t.Fatalf("connect = %v; want the missing-login error", err)
	}
}
