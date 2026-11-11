package commands

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

const privateAliasTenant = "11111111-1111-4111-8111-111111111111"
const privateAliasOtherTenant = "99999999-9999-4999-8999-999999999999"
const privateAliasDevice = "22222222-2222-4222-8222-222222222222"
const privateAliasOtherDevice = "33333333-3333-4333-8333-333333333333"

func privateAliasTestAuth() config.AuthConfig {
	return config.AuthConfig{Name: "default", CloudGRPC: "cloud.example:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + privateAliasTenant + "/operator/44444444-4444-4444-8444-444444444444"}}}
}
func privateAliasTestDevice(t *testing.T, id string) meshid.Device {
	t.Helper()
	tenant, err := meshid.ParseTenant(privateAliasTenant)
	if err != nil {
		t.Fatal(err)
	}
	device, err := meshid.ParseDevice(tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	return device
}
func privateAliasTestLAN(id, name string) models.LANDevice {
	return models.LANDevice{PrincipalURI: certs.DeviceSPIFFEURI(privateAliasTenant, id), TenantUUID: privateAliasTenant, MeshName: name, IPAddress: "192.0.2.10", Port: 50052, IsMTLS: true}
}
func privateAliasTestCloud(id, name string) *cloudpbv2.Asset {
	return &cloudpbv2.Asset{Id: id, Name: name, OrganizationId: privateAliasTenant}
}
func privateAliasTestMesh(t *testing.T, id, name string) sessionbroker.OperatorDirectory {
	t.Helper()
	device := privateAliasTestDevice(t, id)
	return sessionbroker.OperatorDirectory{State: "ready", Captured: time.Now().UnixMilli(), Devices: []sessionbroker.OperatorTargetInfo{{Selector: sessionbroker.MeshSelector(device), Principal: device.Principal(), Tenant: privateAliasTenant, Device: id, Name: name, Expires: time.Now().Add(time.Minute).UnixMilli()}}}
}

func TestPrivateDeviceAliasParsing(t *testing.T) {
	for _, tc := range []struct {
		input          string
		matched, valid bool
		label, uuid    string
	}{
		{"Jetson--A.WENDY.INTERNAL.", true, true, "jetson--a", ""},
		{"device-" + privateAliasDevice + ".wendy.internal", true, true, "device-" + privateAliasDevice, privateAliasDevice},
		{"device-not-a-uuid.wendy.internal", true, true, "device-not-a-uuid", ""},
		{"jetson.mesh.wendy.internal", false, false, "", ""},
		{"jetson.local", false, false, "", ""},
		{"jetson.wendy.internal:50052", true, false, "", ""},
		{"a.b.wendy.internal", true, false, "", ""},
		{"wendy.internal", true, false, "", ""},
		{"Kitchen.wendy.internal", true, false, "", ""},
		{strings.Repeat("a", 64) + ".wendy.internal", true, false, "", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, matched, err := parsePrivateDeviceAlias(tc.input)
			if matched != tc.matched || tc.matched && (err == nil) != tc.valid {
				t.Fatalf("parse=%+v,%v,%v", got, matched, err)
			}
			if tc.valid && (got.label != tc.label || got.uuid != tc.uuid) {
				t.Fatalf("parse=%+v", got)
			}
		})
	}
}

func TestPrivateDeviceAliasLANBrowseRetainsConfirmedRowsOnTimeout(t *testing.T) {
	old := privateAliasLANCollect
	t.Cleanup(func() { privateAliasLANCollect = old })
	device := privateAliasTestLAN(privateAliasDevice, "jetson")
	for _, tc := range []struct {
		name string
		err  error
		keep bool
	}{
		{"complete", nil, true},
		{"deadline", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, true},
		{"discovery failure", errors.New("cannot browse mDNS"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			privateAliasLANCollect = func(_ context.Context, opts discovery.StreamOptions, timeout time.Duration) ([]models.LANDevice, error) {
				if opts.Prober != nil || opts.UseCache || timeout != 3*time.Second {
					t.Fatalf("alias discovery must collect raw live sightings: %+v timeout=%v", opts, timeout)
				}
				return []models.LANDevice{device}, tc.err
			}
			got, err := privateAliasLANBrowse(context.Background())
			if tc.keep {
				if err != nil || !reflect.DeepEqual(got, []models.LANDevice{device}) {
					t.Fatalf("confirmed sighting lost: rows=%+v err=%v", got, err)
				}
			} else if !errors.Is(err, tc.err) || len(got) != 0 {
				t.Fatalf("unexpected discovery failure accepted: rows=%+v err=%v", got, err)
			}
		})
	}
}

func TestPrivateDeviceAliasIdentitySources(t *testing.T) {
	auth := privateAliasTestAuth()
	tenant, _ := meshid.ParseTenant(privateAliasTenant)
	uuidAlias, _, _ := parsePrivateDeviceAlias("device-" + privateAliasDevice + ".wendy.internal")
	friendly, _, _ := parsePrivateDeviceAlias("jetson.wendy.internal")
	expected := &certs.WendyIdentity{TenantUUID: privateAliasTenant, EntityType: certs.EntityAsset, EntityID: privateAliasDevice, Principal: certs.DeviceSPIFFEURI(privateAliasTenant, privateAliasDevice)}
	for _, tc := range []struct {
		name      string
		alias     privateDeviceAlias
		expected  *certs.WendyIdentity
		sources   privateAliasSources
		wantID    string
		wantError string
	}{
		{name: "UUID uses LAN without Cloud", alias: uuidAlias, sources: privateAliasSources{lan: []models.LANDevice{privateAliasTestLAN(privateAliasDevice, "unrelated")}, cloudErr: errors.New("offline")}, wantID: privateAliasDevice},
		{name: "friendly LAN alone cannot establish identity", alias: friendly, sources: privateAliasSources{lan: []models.LANDevice{privateAliasTestLAN(privateAliasDevice, "jetson")}}, wantError: "LAN discovery alone"},
		{name: "saved friendly binding works offline LAN", alias: friendly, expected: expected, sources: privateAliasSources{lan: []models.LANDevice{privateAliasTestLAN(privateAliasDevice, "jetson")}, cloudErr: errors.New("offline")}, wantID: privateAliasDevice},
		{name: "signed mesh binds friendly name", alias: friendly, sources: privateAliasSources{mesh: privateAliasTestMesh(t, privateAliasDevice, "jetson")}, wantID: privateAliasDevice},
		{name: "Cloud binds friendly name", alias: friendly, sources: privateAliasSources{cloud: []*cloudpbv2.Asset{privateAliasTestCloud(privateAliasDevice, "jetson")}}, wantID: privateAliasDevice},
		{name: "all sources agree", alias: friendly, sources: privateAliasSources{mesh: privateAliasTestMesh(t, privateAliasDevice, "jetson"), lan: []models.LANDevice{privateAliasTestLAN(privateAliasDevice, "jetson")}, cloud: []*cloudpbv2.Asset{privateAliasTestCloud(privateAliasDevice, "jetson")}}, wantID: privateAliasDevice},
		{name: "LAN conflicts with signed mesh", alias: friendly, sources: privateAliasSources{mesh: privateAliasTestMesh(t, privateAliasDevice, "jetson"), lan: []models.LANDevice{privateAliasTestLAN(privateAliasOtherDevice, "jetson")}}, wantError: "ambiguous"},
		{name: "Cloud conflicts with saved name", alias: friendly, expected: expected, sources: privateAliasSources{cloud: []*cloudpbv2.Asset{privateAliasTestCloud(privateAliasOtherDevice, "jetson")}}, wantError: "ambiguous"},
		{name: "duplicate Cloud name", alias: friendly, sources: privateAliasSources{cloud: []*cloudpbv2.Asset{privateAliasTestCloud(privateAliasDevice, "jetson"), privateAliasTestCloud(privateAliasOtherDevice, "jetson")}}, wantError: "ambiguous"},
		{name: "repeated hyphens do not collapse", alias: privateDeviceAlias{host: "jetson--a.wendy.internal", label: "jetson--a"}, sources: privateAliasSources{cloud: []*cloudpbv2.Asset{privateAliasTestCloud(privateAliasDevice, "jetson-a"), privateAliasTestCloud(privateAliasOtherDevice, "jetson--a")}}, wantID: privateAliasOtherDevice},
		{name: "reserved UUID cannot be claimed as friendly name", alias: uuidAlias, sources: privateAliasSources{cloud: []*cloudpbv2.Asset{privateAliasTestCloud(privateAliasOtherDevice, "device-"+privateAliasDevice)}}, wantID: privateAliasDevice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectPrivateAliasCandidate(tc.alias, tenant, tc.expected, &auth, tc.sources, time.Now())
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("candidate=%+v err=%v", got, err)
				}
				return
			}
			if err != nil || got.device.UUID().String() != tc.wantID {
				t.Fatalf("candidate=%+v err=%v", got, err)
			}
		})
	}
}

func TestPrivateDeviceAliasRejectsForeignOrMalformedHints(t *testing.T) {
	auth := privateAliasTestAuth()
	tenant, _ := meshid.ParseTenant(privateAliasTenant)
	alias, _, _ := parsePrivateDeviceAlias("jetson.wendy.internal")
	foreignLAN := privateAliasTestLAN(privateAliasDevice, "jetson")
	foreignLAN.PrincipalURI = certs.DeviceSPIFFEURI(privateAliasOtherTenant, privateAliasDevice)
	foreignCloud := privateAliasTestCloud(privateAliasDevice, "jetson")
	foreignCloud.OrganizationId = privateAliasOtherTenant
	for _, mutate := range []func(*privateAliasSources){
		func(s *privateAliasSources) { s.mesh.Devices[0].Tenant = privateAliasOtherTenant },
		func(s *privateAliasSources) { s.mesh.Devices[0].Device = privateAliasOtherDevice },
		func(s *privateAliasSources) {
			s.mesh.Devices[0].Selector = "mesh://" + privateAliasOtherTenant + "/" + privateAliasDevice
		},
		func(s *privateAliasSources) { s.mesh.Devices[0].Expires = time.Now().Add(-time.Second).UnixMilli() },
		func(s *privateAliasSources) { s.mesh.Captured = time.Now().Add(-11 * time.Second).UnixMilli() },
	} {
		sources := privateAliasSources{mesh: privateAliasTestMesh(t, privateAliasDevice, "jetson"), lan: []models.LANDevice{foreignLAN}, cloud: []*cloudpbv2.Asset{foreignCloud}}
		mutate(&sources)
		if got, err := selectPrivateAliasCandidate(alias, tenant, nil, &auth, sources, time.Now()); err == nil {
			t.Fatalf("malformed/foreign discovery accepted %+v", got)
		}
	}
}

func stubPrivateAliasNetwork(t *testing.T, sources privateAliasSources) {
	t.Helper()
	oldMesh, oldLAN, oldCloud := privateAliasMeshRead, privateAliasLANBrowse, privateAliasCloudFetch
	oldPrepare, oldConnect, oldEnforce := privateAliasPrepareAuth, privateAliasConnectMesh, privateAliasEnforcePin
	t.Cleanup(func() {
		privateAliasMeshRead, privateAliasLANBrowse, privateAliasCloudFetch = oldMesh, oldLAN, oldCloud
		privateAliasPrepareAuth, privateAliasConnectMesh, privateAliasEnforcePin = oldPrepare, oldConnect, oldEnforce
	})
	privateAliasMeshRead = func(context.Context) (sessionbroker.OperatorDirectory, error) { return sources.mesh, nil }
	privateAliasLANBrowse = func(context.Context) ([]models.LANDevice, error) { return sources.lan, nil }
	privateAliasCloudFetch = func(ctx context.Context, auth *config.AuthConfig, onlineOnly bool) ([]*cloudpbv2.Asset, error) {
		if auth.Certificates[0].TenantUUID() != privateAliasTenant || onlineOnly {
			t.Error("inventory scope changed")
		}
		return sources.cloud, sources.cloudErr
	}
	privateAliasPrepareAuth = func(context.Context, *config.AuthConfig) error { return nil }
	privateAliasEnforcePin = func(host, addr string, conn *grpcclient.AgentConnection) error {
		if !strings.HasSuffix(host, ".wendy.internal") {
			t.Errorf("wrong pin key %q", host)
		}
		return nil
	}
	privateAliasConnectMesh = func(context.Context, string, bool) (*SelectedDevice, bool, error) {
		t.Error("unexpected mesh connection")
		return nil, true, errors.New("unexpected mesh connection")
	}
}
func privateAliasConfig(t *testing.T, pins map[string]config.DevicePin) {
	t.Helper()
	auth := privateAliasTestAuth()
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{auth}, CurrentContext: "default", DefaultDevice: "preserve.local", DevicePins: pins})
}

func TestPrivateDeviceAliasFrontDoorLANUsesExplicitIdentity(t *testing.T) {
	privateAliasConfig(t, nil)
	stubPrivateAliasNetwork(t, privateAliasSources{lan: []models.LANDevice{privateAliasTestLAN(privateAliasDevice, "jetson")}, cloudErr: errors.New("offline")})
	oldDial := dialAgentLadderWithCertsFn
	t.Cleanup(func() { dialAgentLadderWithCertsFn = oldDial })
	calls := 0
	dialAgentLadderWithCertsFn = func(ctx context.Context, target dialTarget, certificates []config.CertificateInfo) (*grpcclient.AgentConnection, error, error) {
		calls++
		if target.Expected == nil || target.Expected.Principal != certs.DeviceSPIFFEURI(privateAliasTenant, privateAliasDevice) || len(certificates) != 1 || certificates[0].TenantUUID() != privateAliasTenant {
			t.Fatalf("unconstrained alias dial %+v", target)
		}
		if target.Addr != "192.0.2.10:50051" {
			t.Fatalf("unexpected LAN address %q", target.Addr)
		}
		return &grpcclient.AgentConnection{IsMTLS: true}, nil, nil
	}
	host := "device-" + privateAliasDevice + ".wendy.internal"
	selected, matched, err := connectNamedDeviceSelector(context.Background(), host, true)
	if err != nil || !matched || selected == nil || calls != 1 || selected.PinKey != host {
		t.Fatalf("selection=%+v matched=%v err=%v calls=%d", selected, matched, err, calls)
	}
	cfg, _ := config.Load()
	if cfg.DefaultDevice != "preserve.local" || len(cfg.DevicePins) != 0 {
		t.Fatal("alias discovery changed configuration")
	}
}

func TestPrivateDeviceAliasFrontDoorMeshAndCloud(t *testing.T) {
	for _, transport := range []string{"mesh", "cloud"} {
		t.Run(transport, func(t *testing.T) {
			privateAliasConfig(t, nil)
			source := privateAliasSources{}
			if transport == "mesh" {
				source.mesh = privateAliasTestMesh(t, privateAliasDevice, "jetson")
			} else {
				source.cloud = []*cloudpbv2.Asset{privateAliasTestCloud(privateAliasDevice, "jetson")}
			}
			stubPrivateAliasNetwork(t, source)
			calls := 0
			if transport == "mesh" {
				privateAliasConnectMesh = func(ctx context.Context, selector string, suppress bool) (*SelectedDevice, bool, error) {
					calls++
					if selector != "mesh://"+privateAliasTenant+"/"+privateAliasDevice || !suppress {
						t.Fatal("wrong canonical mesh target", selector)
					}
					return &SelectedDevice{Agent: &grpcclient.AgentConnection{IsMTLS: true}, DefaultSelector: selector}, true, nil
				}
			} else {
				old := connectDefaultCloudAssetV2Fn
				t.Cleanup(func() { connectDefaultCloudAssetV2Fn = old })
				connectDefaultCloudAssetV2Fn = func(ctx context.Context, auth *config.AuthConfig, asset *cloudpbv2.Asset, broker string) (*grpcclient.AgentConnection, error) {
					calls++
					if auth.Certificates[0].TenantUUID() != privateAliasTenant || asset.GetId() != privateAliasDevice {
						t.Fatal("wrong Cloud identity")
					}
					return &grpcclient.AgentConnection{IsMTLS: true}, nil
				}
			}
			selected, matched, err := connectNamedDeviceSelector(context.Background(), "jetson.wendy.internal", true)
			if err != nil || !matched || selected == nil || calls != 1 {
				t.Fatalf("selection=%+v %v %v calls=%d", selected, matched, err, calls)
			}
		})
	}
}

func TestPrivateDeviceAliasRefreshesCloudCredentialsBeforeInventory(t *testing.T) {
	privateAliasConfig(t, nil)
	stubPrivateAliasNetwork(t, privateAliasSources{})
	prepared := false
	privateAliasPrepareAuth = func(_ context.Context, auth *config.AuthConfig) error {
		if auth.Certificates[0].TenantUUID() != privateAliasTenant {
			t.Error("certificate preparation changed organization")
		}
		prepared = true
		return nil
	}
	privateAliasCloudFetch = func(_ context.Context, _ *config.AuthConfig, _ bool) ([]*cloudpbv2.Asset, error) {
		if !prepared {
			return nil, errors.New("TLS client certificate expired")
		}
		return []*cloudpbv2.Asset{privateAliasTestCloud(privateAliasDevice, "jetson")}, nil
	}
	old := connectDefaultCloudAssetV2Fn
	t.Cleanup(func() { connectDefaultCloudAssetV2Fn = old })
	connectDefaultCloudAssetV2Fn = func(_ context.Context, auth *config.AuthConfig, asset *cloudpbv2.Asset, _ string) (*grpcclient.AgentConnection, error) {
		if !prepared || auth.Certificates[0].TenantUUID() != privateAliasTenant || asset.GetId() != privateAliasDevice {
			t.Fatal("Cloud connection did not use prepared selected-organization inventory")
		}
		return &grpcclient.AgentConnection{IsMTLS: true}, nil
	}
	selected, matched, err := connectNamedDeviceSelector(context.Background(), "jetson.wendy.internal", true)
	if err != nil || !matched || selected == nil {
		t.Fatalf("refreshable Cloud-only alias failed: prepared=%v matched=%v err=%v", prepared, matched, err)
	}
}

func TestPrivateDeviceAliasCloudPreparationFailureDoesNotListInventory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*config.AuthConfig) error
	}{
		{"renewal refused", func(*config.AuthConfig) error { return errors.New("renewal refused") }},
		{"organization changed", func(auth *config.AuthConfig) error {
			auth.Certificates[0].PrincipalURI = certs.DeviceSPIFFEURI(privateAliasOtherTenant, privateAliasDevice)
			return nil
		}},
		{"certificate removed", func(auth *config.AuthConfig) error { auth.Certificates = nil; return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubPrivateAliasNetwork(t, privateAliasSources{lan: []models.LANDevice{privateAliasTestLAN(privateAliasDevice, "jetson")}})
			privateAliasPrepareAuth = func(_ context.Context, auth *config.AuthConfig) error { return tc.prepare(auth) }
			listed := false
			privateAliasCloudFetch = func(context.Context, *config.AuthConfig, bool) ([]*cloudpbv2.Asset, error) {
				listed = true
				return nil, nil
			}
			auth := privateAliasTestAuth()
			sources := gatherPrivateAliasSources(context.Background(), &auth)
			if listed || sources.cloudErr == nil || len(sources.lan) != 1 {
				t.Fatalf("unprepared inventory listed or independent LAN discovery lost: listed=%v sources=%+v", listed, sources)
			}
		})
	}
}

func TestPrivateDeviceAliasRejectsTruncatedFriendlyMeshDirectory(t *testing.T) {
	auth := privateAliasTestAuth()
	tenant, _ := meshid.ParseTenant(privateAliasTenant)
	friendly, _, _ := parsePrivateDeviceAlias("jetson.wendy.internal")
	uuidAlias, _, _ := parsePrivateDeviceAlias("device-" + privateAliasDevice + ".wendy.internal")
	snapshot := privateAliasTestMesh(t, privateAliasDevice, "jetson")
	snapshot.Truncated = true
	for _, tc := range []struct {
		name    string
		alias   privateDeviceAlias
		cloud   []*cloudpbv2.Asset
		wantErr bool
	}{
		{"friendly mesh alone", friendly, nil, true},
		{"Cloud cannot exclude hidden mesh collision", friendly, []*cloudpbv2.Asset{privateAliasTestCloud(privateAliasDevice, "jetson")}, true},
		{"UUID remains usable", uuidAlias, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, err := selectPrivateAliasCandidate(tc.alias, tenant, nil, &auth, privateAliasSources{mesh: snapshot, cloud: tc.cloud}, time.Now())
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "incomplete mesh directory") {
					t.Fatalf("incomplete collision authority accepted: %+v err=%v", candidate, err)
				}
			} else if err != nil || candidate.mesh != snapshot.Devices[0].Selector {
				t.Fatalf("explicit UUID route lost: %+v err=%v", candidate, err)
			}
		})
	}
}

func TestPrivateDeviceAliasSavedBindingRefusalPrecedesDiscovery(t *testing.T) {
	privateAliasConfig(t, map[string]config.DevicePin{"jetson.wendy.internal": {Principal: certs.DeviceSPIFFEURI(privateAliasOtherTenant, privateAliasDevice)}})
	stubPrivateAliasNetwork(t, privateAliasSources{})
	var called atomic.Bool
	privateAliasLANBrowse = func(context.Context) ([]models.LANDevice, error) { called.Store(true); return nil, nil }
	if _, matched, err := connectNamedDeviceSelector(context.Background(), "jetson.wendy.internal", true); !matched || !errors.Is(err, errDeviceIdentityRefused) || called.Load() {
		t.Fatalf("foreign saved binding used discovery: matched=%v err=%v called=%v", matched, err, called.Load())
	}
}

func TestPrivateDeviceAliasParallelDiscoveryAndCancellation(t *testing.T) {
	privateAliasConfig(t, nil)
	stubPrivateAliasNetwork(t, privateAliasSources{})
	started := make(chan struct{}, 3)
	wait := func(ctx context.Context) { started <- struct{}{}; <-ctx.Done() }
	privateAliasMeshRead = func(ctx context.Context) (sessionbroker.OperatorDirectory, error) {
		wait(ctx)
		return sessionbroker.OperatorDirectory{}, ctx.Err()
	}
	privateAliasLANBrowse = func(ctx context.Context) ([]models.LANDevice, error) { wait(ctx); return nil, ctx.Err() }
	privateAliasCloudFetch = func(ctx context.Context, _ *config.AuthConfig, _ bool) ([]*cloudpbv2.Asset, error) {
		wait(ctx)
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := connectNamedDeviceSelector(ctx, "jetson.wendy.internal", true); done <- err }()
	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("discovery sources were sequential")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("alias lookup ignored cancellation")
	}
}

func TestPrivateDeviceAliasLANRefusalDoesNotFallbackToCloud(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failure   error
		wantCloud bool
	}{
		{"silent endpoint", &noAuthenticatedEndpointError{msg: "unreachable"}, true},
		{"wrong identity", errDeviceIdentityRefused, false},
		{"certificate rejected", &noAuthenticatedEndpointError{msg: "certificate", certSeen: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			privateAliasConfig(t, nil)
			stubPrivateAliasNetwork(t, privateAliasSources{lan: []models.LANDevice{privateAliasTestLAN(privateAliasDevice, "jetson")}, cloud: []*cloudpbv2.Asset{privateAliasTestCloud(privateAliasDevice, "jetson")}})
			oldDial, oldCloud := dialAgentLadderWithCertsFn, connectDefaultCloudAssetV2Fn
			t.Cleanup(func() { dialAgentLadderWithCertsFn, connectDefaultCloudAssetV2Fn = oldDial, oldCloud })
			dialAgentLadderWithCertsFn = func(context.Context, dialTarget, []config.CertificateInfo) (*grpcclient.AgentConnection, error, error) {
				return nil, nil, tc.failure
			}
			cloudCalled := false
			connectDefaultCloudAssetV2Fn = func(context.Context, *config.AuthConfig, *cloudpbv2.Asset, string) (*grpcclient.AgentConnection, error) {
				cloudCalled = true
				return &grpcclient.AgentConnection{IsMTLS: true}, nil
			}
			_, _, err := connectNamedDeviceSelector(context.Background(), "jetson.wendy.internal", true)
			if cloudCalled != tc.wantCloud {
				t.Fatalf("Cloud called=%v err=%v", cloudCalled, err)
			}
			if !tc.wantCloud && !reflect.DeepEqual(err, tc.failure) {
				t.Fatalf("refusal changed: %v", err)
			}
		})
	}
}
