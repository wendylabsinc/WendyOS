package mcp

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
)

type fakeCloudAssetServer struct {
	cloudpb.UnimplementedAssetServiceServer
	assets []*cloudpb.Asset
	// offlineAssets is served instead of assets when the request has
	// OnlineOnly unset/false, modeling the offline-device re-query pickCloudAsset
	// performs when the online-only lookup finds no match.
	offlineAssets []*cloudpb.Asset
	// offlineErr, when set, is returned instead of serving offlineAssets for
	// a query with OnlineOnly unset/false — models a transport failure on
	// pickCloudAsset's offline-inclusive re-query.
	offlineErr error
	pageSize   int                          // when nonzero, emulate server-side pagination
	req        *cloudpb.ListAssetsRequest   // last request, kept for existing single-request assertions
	reqs       []*cloudpb.ListAssetsRequest // every request received, in order
}

func (s *fakeCloudAssetServer) ListAssets(req *cloudpb.ListAssetsRequest, stream grpc.ServerStreamingServer[cloudpb.ListAssetsResponse]) error {
	s.req = req
	s.reqs = append(s.reqs, req)
	if !req.GetOnlineOnly() && s.offlineErr != nil {
		return s.offlineErr
	}
	assets := s.assets
	if !req.GetOnlineOnly() {
		assets = s.offlineAssets
	}
	var total int32
	if s.pageSize > 0 {
		total = int32(len(assets))
		start := min(int(req.GetOffset()), len(assets))
		end := min(start+s.pageSize, len(assets))
		assets = assets[start:end]
	}
	for _, a := range assets {
		if err := stream.Send(&cloudpb.ListAssetsResponse{Asset: a, Total: total}); err != nil {
			return err
		}
	}
	return nil
}

func TestMCPListCloudAssetsPaginates(t *testing.T) {
	for _, onlineOnly := range []bool{true, false} {
		name := "all enrolled devices"
		if onlineOnly {
			name = "online devices"
		}
		t.Run(name, func(t *testing.T) {
			assets := []*cloudpb.Asset{{Id: 1}, {Id: 2}, {Id: 3}, {Id: 4}, {Id: 5}}
			fake := &fakeCloudAssetServer{assets: assets, offlineAssets: assets, pageSize: 2}
			auth := &config.AuthConfig{
				CloudGRPC:    startFakeCloudAssetServer(t, fake),
				Certificates: []config.CertificateInfo{{OrganizationID: 7}},
			}
			got, err := mcpListCloudAssets(context.Background(), auth, "test-filter", onlineOnly)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(assets) {
				t.Fatalf("got %d assets, want all %d", len(got), len(assets))
			}
			for i, asset := range got {
				if asset.GetId() != assets[i].GetId() {
					t.Fatalf("asset %d = %d, want %d", i, asset.GetId(), assets[i].GetId())
				}
			}
			if len(fake.reqs) != 3 {
				t.Fatalf("made %d requests, want 3 pages", len(fake.reqs))
			}
			for i, req := range fake.reqs {
				if req.GetOffset() != int32(i*2) || req.GetOrganizationId() != 7 ||
					req.GetFilter() != "test-filter" || req.GetOnlineOnly() != onlineOnly || !req.GetIsComputeDevice() {
					t.Errorf("page %d lost its offset or filters: %v", i, req)
				}
			}
		})
	}
}

func startFakeCloudAssetServer(t *testing.T, svc *fakeCloudAssetServer) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	g := grpc.NewServer()
	cloudpb.RegisterAssetServiceServer(g, svc)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(func() { g.Stop() })
	return ln.Addr().String()
}

func TestCloudDiscover_ReturnsConfiguredCloudDevices(t *testing.T) {
	fake := &fakeCloudAssetServer{
		assets: []*cloudpb.Asset{
			{
				Id:              42,
				OrganizationId:  7,
				Name:            "edge-one",
				AssetType:       "device",
				IsComputeDevice: true,
			},
		},
	}
	addr := startFakeCloudAssetServer(t, fake)
	srv := New(&config.Config{
		Auth: []config.AuthConfig{{
			CloudGRPC: addr,
			Certificates: []config.CertificateInfo{{
				OrganizationID: 7,
			}},
		}},
	}, nil)

	result, err := srv.callTool(context.Background(), "cloud_discover", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	devices := listPayload(t, result, "devices")
	if len(devices) != 1 {
		t.Fatalf("len(devices) = %d, want 1", len(devices))
	}
	if devices[0]["name"] != "edge-one" {
		t.Errorf("name = %v, want edge-one", devices[0]["name"])
	}
	if fake.req == nil || fake.req.GetOrganizationId() != 7 {
		t.Fatalf("ListAssets organization_id = %v, want 7", fake.req)
	}
	if !fake.req.GetIsComputeDevice() {
		t.Fatal("ListAssets did not request compute devices")
	}
}

func TestCloudDiscover_HasStructuredContent(t *testing.T) {
	fake := &fakeCloudAssetServer{
		assets: []*cloudpb.Asset{
			{
				Id:              42,
				OrganizationId:  7,
				Name:            "edge-one",
				AssetType:       "device",
				IsComputeDevice: true,
			},
		},
	}
	addr := startFakeCloudAssetServer(t, fake)
	srv := New(&config.Config{
		Auth: []config.AuthConfig{{
			CloudGRPC: addr,
			Certificates: []config.CertificateInfo{{
				OrganizationID: 7,
			}},
		}},
	}, nil)

	result, err := srv.callTool(context.Background(), "cloud_discover", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	if _, ok := structuredMap(t, result)["devices"]; !ok {
		t.Error("cloud_discover envelope is missing the devices key")
	}
}

func TestCloudDiscover_MaxBytesTruncates(t *testing.T) {
	assets := make([]*cloudpb.Asset, 0, 200)
	for i := int32(0); i < 200; i++ {
		assets = append(assets, &cloudpb.Asset{
			Id:              i,
			OrganizationId:  7,
			Name:            "some-padding-device-name",
			AssetType:       "device",
			IsComputeDevice: true,
		})
	}
	fake := &fakeCloudAssetServer{assets: assets}
	addr := startFakeCloudAssetServer(t, fake)
	srv := New(&config.Config{
		Auth: []config.AuthConfig{{
			CloudGRPC: addr,
			Certificates: []config.CertificateInfo{{
				OrganizationID: 7,
			}},
		}},
	}, nil)

	result, err := srv.callTool(context.Background(), "cloud_discover", map[string]any{"max_bytes": 50})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("truncation is not an error result: %v", result.Content)
	}
	sc, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent has unexpected type %T", result.StructuredContent)
	}
	if sc["truncated"] != true {
		t.Errorf("expected truncated=true, got %v", sc["truncated"])
	}
}

func TestCloud_MultipleSessions_Code(t *testing.T) {
	srv := New(&config.Config{
		Auth: []config.AuthConfig{
			{CloudGRPC: "one:123", Certificates: []config.CertificateInfo{{OrganizationID: 1}}},
			{CloudGRPC: "two:123", Certificates: []config.CertificateInfo{{OrganizationID: 1}}},
		},
	}, nil)
	result, err := srv.callTool(context.Background(), "cloud_discover", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result")
	}
	sc, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected map structured content, got %T", result.StructuredContent)
	}
	if sc["error_code"] != string(errCodeMultipleSessions) {
		t.Errorf("error_code = %v, want %s", sc["error_code"], errCodeMultipleSessions)
	}
}

func TestCloudDiscover_RequiresAuth(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "cloud_discover", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result")
	}
}

func TestCloudDiscover_RequiresCloudGRPCWhenMultipleAuthSessionsExist(t *testing.T) {
	srv := New(&config.Config{
		Auth: []config.AuthConfig{
			{CloudGRPC: "one:123", Certificates: []config.CertificateInfo{{OrganizationID: 1}}},
			{CloudGRPC: "two:123", Certificates: []config.CertificateInfo{{OrganizationID: 1}}},
		},
	}, nil)
	result, err := srv.callTool(context.Background(), "cloud_discover", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result")
	}
}

func TestRun_MissingProjectPath(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "run", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true when project_path is missing")
	}
}

func TestCloudTunnel_RejectsUnknownProtocol(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "cloud_tunnel", map[string]any{
		"local_port": 8080,
		"protocol":   "quic",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for unknown protocol")
	}
	sc := structuredMap(t, result)
	if sc["error_code"] != string(errCodeInvalidArgument) {
		t.Errorf("error_code = %v, want %s", sc["error_code"], errCodeInvalidArgument)
	}
}

func TestCloudTunnel_DefaultsProtocolToTCP(t *testing.T) {
	// Without a "protocol" argument at all, the invalid-argument short-circuit
	// must not fire — the request should proceed past port/protocol
	// validation and fail later for lack of auth, not for protocol.
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "cloud_tunnel", map[string]any{
		"local_port": 8080,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result (no auth configured)")
	}
	sc := structuredMap(t, result)
	if sc["error_code"] == string(errCodeInvalidArgument) {
		t.Errorf("expected the failure to come from auth resolution, not protocol validation; got %v", sc)
	}
}

func TestCloudTunnel_RejectsOutOfRangeLocalPort(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "cloud_tunnel", map[string]any{
		"local_port": 70000,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for out-of-range local_port")
	}
	sc := structuredMap(t, result)
	if sc["error_code"] != string(errCodeInvalidArgument) {
		t.Errorf("error_code = %v, want %s", sc["error_code"], errCodeInvalidArgument)
	}
}

func TestCloudPing_RequiresDeviceName(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "cloud_ping", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result when device_name is missing")
	}
	sc := structuredMap(t, result)
	if sc["error_code"] != string(errCodeInvalidArgument) {
		t.Errorf("error_code = %v, want %s", sc["error_code"], errCodeInvalidArgument)
	}
}

func TestCloudPing_RejectsCountAboveMax(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "cloud_ping", map[string]any{
		"device_name": "edge-one",
		"count":       21,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for count above max (20)")
	}
	sc := structuredMap(t, result)
	if sc["error_code"] != string(errCodeInvalidArgument) {
		t.Errorf("error_code = %v, want %s", sc["error_code"], errCodeInvalidArgument)
	}
}

func TestCloudPing_RejectsCountBelowMin(t *testing.T) {
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "cloud_ping", map[string]any{
		"device_name": "edge-one",
		"count":       0,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for count below min (1)")
	}
	sc := structuredMap(t, result)
	if sc["error_code"] != string(errCodeInvalidArgument) {
		t.Errorf("error_code = %v, want %s", sc["error_code"], errCodeInvalidArgument)
	}
}

func TestCloudPing_DefaultCountWithinBounds(t *testing.T) {
	// No count argument at all should use the default (4), which is within
	// bounds — the failure here must come from auth resolution, not count
	// validation.
	srv := New(&config.Config{}, nil)
	result, err := srv.callTool(context.Background(), "cloud_ping", map[string]any{
		"device_name": "edge-one",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result (no auth configured)")
	}
	sc := structuredMap(t, result)
	if sc["message"] == "count must be between 1 and 20" {
		t.Errorf("default count should not trigger count validation; got %v", sc)
	}
}

func TestCloudAuthEntry_UsesDefaultWhenMultiple(t *testing.T) {
	srv := New(&config.Config{
		DefaultCloudGRPC: "two:123",
		Auth: []config.AuthConfig{
			{CloudGRPC: "one:123", Certificates: []config.CertificateInfo{{OrganizationID: 1}}},
			{CloudGRPC: "two:123", Certificates: []config.CertificateInfo{{OrganizationID: 2}}},
		},
	}, nil)
	auth, err := srv.cloudAuthEntry("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth.CloudGRPC != "two:123" {
		t.Fatalf("want default session two:123, got %s", auth.CloudGRPC)
	}
}

func TestCloudAuthEntry_ErrorsMentionsCloudGRPCParam(t *testing.T) {
	srv := New(&config.Config{
		Auth: []config.AuthConfig{
			{CloudGRPC: "one:123", Certificates: []config.CertificateInfo{{OrganizationID: 1}}},
			{CloudGRPC: "two:123", Certificates: []config.CertificateInfo{{OrganizationID: 2}}},
		},
	}, nil)
	_, err := srv.cloudAuthEntry("")
	if err == nil || !strings.Contains(err.Error(), "cloud_grpc") {
		t.Fatalf("want error mentioning cloud_grpc param, got %v", err)
	}
}

// TestCloudConnect_OfflineDeviceReportedOffline covers pickCloudAsset's
// "empty-with-name" path: the online-only listing comes back empty, but the
// device turns up in the unfiltered re-query, so the error should say it's
// enrolled-but-offline (DEVICE_UNREACHABLE) rather than claiming the device
// doesn't exist. It must also never reach a broker dial: connectToCloudAgent
// only calls DialBroker after pickCloudAsset succeeds, and a bogus/loopback
// CloudGRPC address here would surface as a different failure if that
// ordering regressed.
func TestCloudConnect_OfflineDeviceReportedOffline(t *testing.T) {
	fake := &fakeCloudAssetServer{
		offlineAssets: []*cloudpb.Asset{
			{
				Id:              42,
				OrganizationId:  7,
				Name:            "edge-one",
				AssetType:       "device",
				IsComputeDevice: true,
			},
		},
	}
	addr := startFakeCloudAssetServer(t, fake)
	srv := New(&config.Config{
		Auth: []config.AuthConfig{{
			CloudGRPC: addr,
			Certificates: []config.CertificateInfo{{
				OrganizationID: 7,
			}},
		}},
	}, nil)

	result, err := srv.callTool(context.Background(), "cloud_connect", map[string]any{"device_name": "edge-one"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result, got: %v", result.Content)
	}
	sc := structuredMap(t, result)
	if sc["error_code"] != string(errCodeDeviceUnreachable) {
		t.Errorf("error_code = %v, want %s", sc["error_code"], errCodeDeviceUnreachable)
	}
	msg, _ := sc["message"].(string)
	if !strings.Contains(msg, "enrolled but currently reported offline") {
		t.Errorf("message = %q, want it to contain %q", msg, "enrolled but currently reported offline")
	}
	if len(fake.reqs) != 2 {
		t.Fatalf("len(fake.reqs) = %d, want 2", len(fake.reqs))
	}
	if !fake.reqs[0].GetOnlineOnly() {
		t.Errorf("reqs[0].OnlineOnly = %v, want true", fake.reqs[0].GetOnlineOnly())
	}
	if fake.reqs[1].GetOnlineOnly() {
		t.Errorf("reqs[1].OnlineOnly = %v, want unset/false", fake.reqs[1].GetOnlineOnly())
	}
}

// TestCloudConnect_UnknownDeviceStaysNotFound covers pickCloudAsset's
// "no-match" path: the online-only listing is non-empty but doesn't contain
// the requested name, and the unfiltered re-query doesn't find it either
// (it isn't enrolled at all), so the original NOT_FOUND message must be
// preserved rather than being replaced with the offline message.
func TestCloudConnect_UnknownDeviceStaysNotFound(t *testing.T) {
	fake := &fakeCloudAssetServer{
		assets: []*cloudpb.Asset{
			{
				Id:              42,
				OrganizationId:  7,
				Name:            "edge-one",
				AssetType:       "device",
				IsComputeDevice: true,
			},
		},
	}
	addr := startFakeCloudAssetServer(t, fake)
	srv := New(&config.Config{
		Auth: []config.AuthConfig{{
			CloudGRPC: addr,
			Certificates: []config.CertificateInfo{{
				OrganizationID: 7,
			}},
		}},
	}, nil)

	result, err := srv.callTool(context.Background(), "cloud_connect", map[string]any{"device_name": "ghost-device"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result, got: %v", result.Content)
	}
	sc := structuredMap(t, result)
	if sc["error_code"] != string(errCodeNotFound) {
		t.Errorf("error_code = %v, want %s", sc["error_code"], errCodeNotFound)
	}
	msg, _ := sc["message"].(string)
	if !strings.Contains(msg, `no device named "ghost-device" found`) {
		t.Errorf("message = %q, want it to contain %s", msg, `no device named "ghost-device" found`)
	}
	if strings.Contains(msg, "offline") {
		t.Errorf("message = %q, should not mention offline for a device that was never enrolled", msg)
	}
}

// TestCloudConnect_MatchesOnlineDeviceByNumericID covers pickCloudAsset's
// primary matching loop gaining the numeric-id fallback resolveCloudAsset
// already has in commands/cloud_tunnel.go. Before this fix, a device_name
// that only matched an asset's numeric id (not its name) missed the
// name-only primary loop entirely and fell through to offlineDeviceErr's
// re-query — which DOES match by id via clouddefaults.FindAssetByNameOrID —
// so an online device targeted by id was permanently reported
// DEVICE_UNREACHABLE ("offline") even though it was online the whole time.
// It must also never reach a broker dial for the wrong reason:
// connectToCloudAgent only calls DialBroker after pickCloudAsset succeeds,
// so this fixture (whose CloudGRPC address isn't a real broker and whose
// certificate is empty) failing at the TLS-assembly stage — rather than at
// resolution — is exactly what proves resolution itself succeeded.
func TestCloudConnect_MatchesOnlineDeviceByNumericID(t *testing.T) {
	device := &cloudpb.Asset{
		Id:              42,
		OrganizationId:  7,
		Name:            "edge-one",
		AssetType:       "device",
		IsComputeDevice: true,
	}
	fake := &fakeCloudAssetServer{
		// The device is genuinely online, so it appears in both the
		// online-only listing and the unfiltered (offline-inclusive) one.
		assets:        []*cloudpb.Asset{device},
		offlineAssets: []*cloudpb.Asset{device},
	}
	addr := startFakeCloudAssetServer(t, fake)
	srv := New(&config.Config{
		Auth: []config.AuthConfig{{
			CloudGRPC: addr,
			Certificates: []config.CertificateInfo{{
				OrganizationID: 7,
			}},
		}},
	}, nil)

	result, err := srv.callTool(context.Background(), "cloud_connect", map[string]any{"device_name": "42"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected an error result (broker dial fails against this fixture), got success: %v", result.Content)
	}
	sc := structuredMap(t, result)
	msg, _ := sc["message"].(string)
	if strings.Contains(msg, "enrolled but currently reported offline") {
		t.Errorf("message = %q, resolution wrongly fell back to reporting an online device (matched by id) as offline", msg)
	}
	if strings.Contains(msg, "no device named") {
		t.Errorf("message = %q, resolution wrongly reported an id-matched device as not found", msg)
	}
	if len(fake.reqs) != 1 {
		t.Errorf("len(fake.reqs) = %d, want 1 (id match in the primary online-only listing; no offline re-query needed)", len(fake.reqs))
	}
}

// TestCloudConnect_EmptyOrgWithDeviceName_ReturnsNamedNotFound covers
// pickCloudAsset's "empty online listing, name given" path: when the
// online-only listing comes back empty and the offline-inclusive re-query
// doesn't find deviceName either, the error must be the named NOT_FOUND
// ("no device named %q found") rather than the org-wide "no enrolled
// devices ... enroll a device" message — that message is only accurate when
// no device_name was given at all, and its enroll guidance misleads when
// the real problem is a typo'd or stale name.
func TestCloudConnect_EmptyOrgWithDeviceName_ReturnsNamedNotFound(t *testing.T) {
	fake := &fakeCloudAssetServer{}
	addr := startFakeCloudAssetServer(t, fake)
	srv := New(&config.Config{
		Auth: []config.AuthConfig{{
			CloudGRPC: addr,
			Certificates: []config.CertificateInfo{{
				OrganizationID: 7,
			}},
		}},
	}, nil)

	result, err := srv.callTool(context.Background(), "cloud_connect", map[string]any{"device_name": "ghost-device"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result, got: %v", result.Content)
	}
	sc := structuredMap(t, result)
	if sc["error_code"] != string(errCodeNotFound) {
		t.Errorf("error_code = %v, want %s", sc["error_code"], errCodeNotFound)
	}
	msg, _ := sc["message"].(string)
	if !strings.Contains(msg, `no device named "ghost-device" found`) {
		t.Errorf("message = %q, want it to contain %s", msg, `no device named "ghost-device" found`)
	}
	if strings.Contains(msg, "enroll a device with cloud_enroll_device") {
		t.Errorf("message = %q, should not fall back to the org-wide enroll-a-device message when a specific device_name was given", msg)
	}
}

// TestCloudConnect_OfflineRequeryFails_KeepsOriginalNotFound covers
// offlineDeviceErr's transport-failure path: when the offline-inclusive
// re-query itself fails (e.g. a transient cloud-API outage), that failure
// must be swallowed rather than surfaced. Surfacing it would route through
// cloudErrResult/codeFromGRPC and, for an Unavailable-shaped status,
// mislabel the outage as DEVICE_UNREACHABLE — which misleadingly implies
// this specific device is known-and-offline, when the lookup itself simply
// couldn't be completed. The caller's original NOT_FOUND must stand
// instead, mirroring upgradeOfflineResolveErr in commands/cloud_tunnel.go.
func TestCloudConnect_OfflineRequeryFails_KeepsOriginalNotFound(t *testing.T) {
	fake := &fakeCloudAssetServer{
		assets: []*cloudpb.Asset{
			{
				Id:              42,
				OrganizationId:  7,
				Name:            "edge-one",
				AssetType:       "device",
				IsComputeDevice: true,
			},
		},
		offlineErr: errors.New("cloud API unavailable"),
	}
	addr := startFakeCloudAssetServer(t, fake)
	srv := New(&config.Config{
		Auth: []config.AuthConfig{{
			CloudGRPC: addr,
			Certificates: []config.CertificateInfo{{
				OrganizationID: 7,
			}},
		}},
	}, nil)

	result, err := srv.callTool(context.Background(), "cloud_connect", map[string]any{"device_name": "ghost-device"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result, got: %v", result.Content)
	}
	sc := structuredMap(t, result)
	if sc["error_code"] != string(errCodeNotFound) {
		t.Errorf("error_code = %v, want %s (the failed offline re-query must not surface as its own error)", sc["error_code"], errCodeNotFound)
	}
	msg, _ := sc["message"].(string)
	if !strings.Contains(msg, `no device named "ghost-device" found`) {
		t.Errorf("message = %q, want it to contain %s", msg, `no device named "ghost-device" found`)
	}
	if len(fake.reqs) != 2 {
		t.Fatalf("len(fake.reqs) = %d, want 2 (online listing, then the failed offline re-query)", len(fake.reqs))
	}
}
