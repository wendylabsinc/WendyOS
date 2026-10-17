package commands

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func unresolvedCloudNameError() error {
	return &net.DNSError{Name: "ci-mesh-2", Err: "no such host", IsNotFound: true}
}

func stubUnresolvedCloudPinSideEffects(t *testing.T) {
	t.Helper()
	t.Setenv(runNoCloudFallbackEnv, "")
	oldCloud, oldFresh, oldUpdate := connectUnresolvedCloudNameFn, ensureFreshCertificateFn, checkAndOfferUpdateFn
	t.Cleanup(func() {
		connectUnresolvedCloudNameFn, ensureFreshCertificateFn, checkAndOfferUpdateFn = oldCloud, oldFresh, oldUpdate
	})
	ensureFreshCertificateFn = func(context.Context, *config.AuthConfig) error { return nil }
	connectUnresolvedCloudNameFn = func(context.Context, *config.AuthConfig, string, string, *certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		t.Fatal("refused Cloud session reached device discovery or connection")
		return nil, nil
	}
	checkAndOfferUpdateFn = func(context.Context, *grpcclient.AgentConnection) (*grpcclient.AgentConnection, error) {
		t.Fatal("refused Cloud session reached the update check")
		return nil, nil
	}
}

func TestUnresolvedCloudNameSelectsPinnedSessionBeforeCurrentContext(t *testing.T) {
	const tenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	const otherTenant = "ed6e09f3-d287-450b-a053-e4554f3c70ed"
	const asset = "7791d76e-a942-4a8e-b582-063d063b5623"
	for _, tc := range []struct {
		name      string
		pin       config.DevicePin
		principal string
		selector  string
		expected  *certs.WendyIdentity
		alias     bool
	}{
		{
			name: "legacy asset pin",
			pin:  config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"}, selector: "42",
			expected: &certs.WendyIdentity{OrgID: 7, EntityType: certs.EntityAsset, EntityID: "42"},
		},
		{
			name: "legacy organization-only pin",
			pin:  config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443"}, selector: "ci-mesh-2",
		},
		{
			name: "tenant pin with zero legacy organization",
			pin: config.DevicePin{CloudGRPC: "grpc.a.sh:443", AssetID: asset,
				Principal: "spiffe://wendy.sh/tenant/" + tenant + "/device/" + asset},
			principal: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test", selector: asset,
			expected: &certs.WendyIdentity{TenantUUID: tenant, EntityType: certs.EntityAsset, EntityID: asset,
				Principal: "spiffe://wendy.sh/tenant/" + tenant + "/device/" + asset},
		},
		{
			name: "cloud pin under cached alias governs over direct LAN pin",
			pin:  config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42", Source: config.PinSourceCloud}, selector: "42",
			expected: &certs.WendyIdentity{OrgID: 7, EntityType: certs.EntityAsset, EntityID: "42"}, alias: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubUnresolvedCloudPinSideEffects(t)
			stubNonInteractive(t)
			pins := map[string]config.DevicePin{"ci-mesh-2": tc.pin}
			if tc.alias {
				setPinCache(t, discoverycache.Entry{ID: "device-42", Hostname: "ci-mesh-2.local", DisplayName: "saved-device"})
				pins = map[string]config.DevicePin{
					"ci-mesh-2":    {OrgID: 9, CloudGRPC: "grpc.b.sh:443", AssetID: "99", Source: config.PinSourceLAN},
					"saved-device": tc.pin,
				}
			} else {
				setPinCache(t)
			}
			matching := config.AuthConfig{Name: "saved", CloudGRPC: tc.pin.CloudGRPC,
				Certificates: []config.CertificateInfo{{OrganizationID: tc.pin.OrgID, PrincipalURI: tc.principal}}}
			otherScope := config.AuthConfig{Name: "current", CloudGRPC: tc.pin.CloudGRPC,
				Certificates: []config.CertificateInfo{{OrganizationID: 9}}}
			if tc.principal != "" {
				otherScope.Certificates = []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + otherTenant + "/operator/test"}}
			}
			otherEndpoint := matching
			otherEndpoint.Name, otherEndpoint.CloudGRPC = "other-cloud", "grpc.b.sh:443"
			setTempConfig(t, &config.Config{
				Auth: []config.AuthConfig{otherScope, otherEndpoint, matching}, DevicePins: pins,
				CurrentContext: otherScope.Name, DefaultCloudGRPC: otherEndpoint.CloudGRPC, DefaultOrgID: 9,
			})
			prepared, connected := false, false
			ensureFreshCertificateFn = func(_ context.Context, selected *config.AuthConfig) error {
				if selected.Name != matching.Name || selected.CloudGRPC != matching.CloudGRPC {
					t.Fatalf("prepared session %q on %q, want saved session", selected.Name, selected.CloudGRPC)
				}
				prepared = true
				return nil
			}
			want := &grpcclient.AgentConnection{Host: "ci-mesh-2", IsMTLS: true}
			connectUnresolvedCloudNameFn = func(_ context.Context, selected *config.AuthConfig, name, _ string, expected *certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
				connected = true
				if !prepared || selected.Name != matching.Name || selected.CloudGRPC != matching.CloudGRPC ||
					selected.Certificates[0].OrganizationID != matching.Certificates[0].OrganizationID ||
					selected.Certificates[0].TenantUUID() != matching.Certificates[0].TenantUUID() || name != tc.selector {
					t.Fatalf("Cloud selection = session %q on %q, selector %q, prepared=%v; want saved session and %q", selected.Name, selected.CloudGRPC, name, prepared, tc.selector)
				}
				if tc.expected == nil {
					if expected != nil {
						t.Fatalf("expected identity = %+v, want nil for organization-only pin", expected)
					}
				} else if expected == nil || *expected != *tc.expected {
					t.Fatalf("expected identity = %+v, want %+v", expected, tc.expected)
				}
				return want, nil
			}
			conn, err, attempted := connectUnresolvedCloudName(context.Background(), "ci-mesh-2", unresolvedCloudNameError(), SuppressUpdateCheck())
			if err != nil || conn != want || !attempted || !connected {
				t.Fatalf("connection = %p, %v, attempted=%v, connected=%v; want saved device", conn, err, attempted, connected)
			}
		})
	}
}

func TestUnresolvedCloudNameRefusesSessionsOutsidePin(t *testing.T) {
	const tenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	const otherTenant = "ed6e09f3-d287-450b-a053-e4554f3c70ed"
	for _, tc := range []struct {
		name string
		pin  config.DevicePin
		auth config.AuthConfig
	}{
		{
			name: "legacy organization-only pin rejects another organization",
			pin:  config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443"},
			auth: config.AuthConfig{CloudGRPC: "grpc.a.sh:443", Certificates: []config.CertificateInfo{{OrganizationID: 9}}},
		},
		{
			name: "same organization and asset on another endpoint",
			pin:  config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
			auth: config.AuthConfig{CloudGRPC: "grpc.b.sh:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}},
		},
		{
			name: "same legacy organization but different tenant",
			pin:  config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42", Principal: "spiffe://wendy.sh/tenant/" + tenant + "/device/test"},
			auth: config.AuthConfig{CloudGRPC: "grpc.a.sh:443", Certificates: []config.CertificateInfo{{OrganizationID: 7, PrincipalURI: "spiffe://wendy.sh/tenant/" + otherTenant + "/operator/test"}}},
		},
		{
			name: "zero legacy organization cannot match another tenant",
			pin:  config.DevicePin{CloudGRPC: "grpc.a.sh:443", AssetID: "42", Principal: "spiffe://wendy.sh/tenant/" + tenant + "/device/test"},
			auth: config.AuthConfig{CloudGRPC: "grpc.a.sh:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + otherTenant + "/operator/test"}}},
		},
		{
			name: "tenant pin cannot use legacy-only session",
			pin:  config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42", Principal: "spiffe://wendy.sh/tenant/" + tenant + "/device/test"},
			auth: config.AuthConfig{CloudGRPC: "grpc.a.sh:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}},
		},
		{
			name: "legacy pin cannot use tenant session with same organization field",
			pin:  config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
			auth: config.AuthConfig{CloudGRPC: "grpc.a.sh:443", Certificates: []config.CertificateInfo{{OrganizationID: 7, PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test"}}},
		},
		{
			name: "malformed principal cannot fall back to legacy organization",
			pin:  config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42", Principal: "invalid-principal"},
			auth: config.AuthConfig{CloudGRPC: "grpc.a.sh:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubUnresolvedCloudPinSideEffects(t)
			setPinCache(t)
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{tc.auth}, DevicePins: map[string]config.DevicePin{"ci-mesh-2": tc.pin}})
			ensureFreshCertificateFn = func(context.Context, *config.AuthConfig) error {
				t.Fatal("unrelated session reached certificate preparation")
				return nil
			}
			conn, err, attempted := connectUnresolvedCloudName(context.Background(), "ci-mesh-2", unresolvedCloudNameError())
			if conn != nil || !attempted || !errors.Is(err, errDeviceIdentityRefused) {
				t.Fatalf("connection = %v, %v, attempted=%v; want identity refusal", conn, err, attempted)
			}
		})
	}
}

func TestUnresolvedCloudNameRechecksPinAfterCertificatePreparation(t *testing.T) {
	const tenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	const otherTenant = "ed6e09f3-d287-450b-a053-e4554f3c70ed"
	for _, change := range []string{"organization", "endpoint", "tenant"} {
		t.Run(change, func(t *testing.T) {
			stubUnresolvedCloudPinSideEffects(t)
			setPinCache(t)
			pin := config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"}
			auth := config.AuthConfig{CloudGRPC: pin.CloudGRPC, Certificates: []config.CertificateInfo{{OrganizationID: 7}}}
			if change == "tenant" {
				pin.Principal = "spiffe://wendy.sh/tenant/" + tenant + "/device/test"
				auth.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/" + tenant + "/operator/test"
			}
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{auth}, DevicePins: map[string]config.DevicePin{"ci-mesh-2": pin}})
			prepared := false
			ensureFreshCertificateFn = func(_ context.Context, selected *config.AuthConfig) error {
				prepared = true
				switch change {
				case "organization":
					selected.Certificates[0].OrganizationID = 9
				case "endpoint":
					selected.CloudGRPC = "grpc.b.sh:443"
				case "tenant":
					selected.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/" + otherTenant + "/operator/test"
				}
				return nil
			}
			conn, err, attempted := connectUnresolvedCloudName(context.Background(), "ci-mesh-2", unresolvedCloudNameError())
			if !prepared || conn != nil || !attempted || !errors.Is(err, errDeviceIdentityRefused) {
				t.Fatalf("connection = %v, %v, attempted=%v, prepared=%v; want identity refusal after preparation", conn, err, attempted, prepared)
			}
		})
	}
}

func TestNamedCloudDeviceResolvesThroughBothAgentEntryPoints(t *testing.T) {
	for _, entry := range []string{"connectToAgent", "resolveTarget"} {
		t.Run(entry, func(t *testing.T) {
			restoreDeviceGlobals(t)
			stubNonInteractive(t)
			setPinCache(t)
			writePinTestConfig(t, nil)
			t.Setenv("WENDY_AGENT_SOCKET", "")
			t.Setenv(runNoCloudFallbackEnv, "")
			deviceFlag = "ci-mesh-2"
			oldLookup, oldBrowse, oldDiscover := osLookupHostFn, lanBrowseFn, discoverLANDevices
			oldUSB, oldFind := usbDirectCandidatesFn, findDeviceByIDFn
			oldCloud := connectUnresolvedCloudNameFn
			t.Cleanup(func() {
				osLookupHostFn, lanBrowseFn, discoverLANDevices = oldLookup, oldBrowse, oldDiscover
				usbDirectCandidatesFn, findDeviceByIDFn = oldUSB, oldFind
				connectUnresolvedCloudNameFn = oldCloud
			})
			osLookupHostFn = func(context.Context, string) ([]string, error) { return nil, unresolvedCloudNameError() }
			lanBrowseFn = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
			discoverLANDevices = lanBrowseFn
			usbDirectCandidatesFn = func() []discovery.USBDirectCandidate { return nil }
			findDeviceByIDFn = func(context.Context, string) *SelectedDevice { return nil }
			want := &grpcclient.AgentConnection{Host: "ci-mesh-2", IsMTLS: true}
			calls := 0
			connectUnresolvedCloudNameFn = func(_ context.Context, _ *config.AuthConfig, name, _ string, expected *certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
				calls++
				if name != deviceFlag || expected != nil {
					t.Fatalf("cloud selection = %q, %+v; want the explicitly named device", name, expected)
				}
				return want, nil
			}
			opts := []resolveOption{SuppressUpdateCheck(), SuppressProvisioningHint(), DisableSessionBroker(), NonInteractive(), ExcludeProviders("local")}
			var got *grpcclient.AgentConnection
			var err error
			if entry == "connectToAgent" {
				got, err = connectToAgent(context.Background(), opts...)
			} else {
				var selected *SelectedDevice
				selected, err = resolveTarget(context.Background(), opts...)
				if selected != nil {
					got = selected.Agent
				}
			}
			if err != nil || got != want || calls != 1 {
				t.Fatalf("connection = %p, %v; cloud calls=%d", got, err, calls)
			}
		})
	}
}

func TestUnresolvedCloudNamePreservesPinnedIdentityAndCloudErrors(t *testing.T) {
	setPinCache(t)
	writePinTestConfig(t, map[string]config.DevicePin{
		"ci-mesh-2": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
	})
	t.Setenv(runNoCloudFallbackEnv, "")
	old := connectUnresolvedCloudNameFn
	t.Cleanup(func() { connectUnresolvedCloudNameFn = old })
	for _, want := range []error{
		&errCloudDeviceNotFound{name: "ci-mesh-2"},
		commandErrorf(errDeviceAmbiguous, "multiple devices match"),
		commandErrorf(errDeviceOffline, "device is enrolled but offline"),
		refuseIdentity("saved device identity changed"),
	} {
		connectUnresolvedCloudNameFn = func(_ context.Context, _ *config.AuthConfig, name, _ string, expected *certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
			if name != "42" || expected == nil || expected.EntityID != "42" || expected.OrgID != 7 {
				t.Fatalf("cloud selection = %q, %+v; want pinned asset 42 in org 7", name, expected)
			}
			return nil, want
		}
		conn, err, attempted := connectUnresolvedCloudName(context.Background(), "ci-mesh-2", unresolvedCloudNameError())
		if conn != nil || !attempted || !errors.Is(err, want) {
			t.Fatalf("connection = %v, %v, attempted=%v; want original Cloud error %v", conn, err, attempted, want)
		}
	}
}

func TestUnresolvedCloudNameNeverReinterpretsEndpointsOrRefusals(t *testing.T) {
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{{CloudGRPC: "cloud.example:443"}}})
	t.Setenv(runNoCloudFallbackEnv, "")
	old := connectUnresolvedCloudNameFn
	t.Cleanup(func() { connectUnresolvedCloudNameFn = old })
	connectUnresolvedCloudNameFn = func(context.Context, *config.AuthConfig, string, string, *certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		t.Fatal("Cloud must not replace the requested endpoint or bypass a refusal")
		return nil, nil
	}
	for _, device := range []string{"", "192.168.100.111", "ci-mesh-2:50051", "ci-mesh-2.local", "ci-mesh-2.example.com", "[::1]:50051", "vm:ci-mesh-2", "cloud://cloud.example/tenant/t/asset/a"} {
		if _, _, attempted := connectUnresolvedCloudName(context.Background(), device, unresolvedCloudNameError()); attempted {
			t.Errorf("attempted Cloud lookup for %q", device)
		}
	}
	for _, cause := range []error{context.Canceled, errDeviceIdentityRefused, errProvisionedAgentUnauthorized, errTLSHandshakeRejected} {
		// Include a resolver miss to prove the refusal wins over that evidence.
		if _, _, attempted := connectUnresolvedCloudName(context.Background(), "ci-mesh-2", errors.Join(cause, unresolvedCloudNameError())); attempted {
			t.Errorf("attempted Cloud lookup after %v", cause)
		}
	}
	if _, _, attempted := connectUnresolvedCloudName(context.Background(), "ci-mesh-2", errors.New("connection refused")); attempted {
		t.Error("a failed resolved endpoint fell back to Cloud")
	}
	t.Setenv(runNoCloudFallbackEnv, "1")
	if _, _, attempted := connectUnresolvedCloudName(context.Background(), "ci-mesh-2", unresolvedCloudNameError()); attempted {
		t.Error("MCP-pinned target fell back to Cloud")
	}
}
