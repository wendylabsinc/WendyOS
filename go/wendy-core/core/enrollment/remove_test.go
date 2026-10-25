package enrollment

import (
	"context"
	"fmt"
	cloudpbv2 "github.com/wendylabsinc/WendyOS/go/wendy-core/core/enrollment/internal/cloudpb"
	pkicorepb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/enrollment/internal/pkipb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
	"reflect"
	"strings"
	"testing"
)

const (
	testTenant  = "2558fd76-afc7-466e-9613-6b715296a526"
	testAsset   = "6fab25c6-01d3-4840-abcc-9d47259069a4"
	testDevice  = "lite-441bf6804824"
	firstToken  = "ccf9d7eb-f72e-483a-a6d7-deed7169e322"
	secondToken = "793c38e2-986d-48b9-a368-d028f15da784"
	testEAB     = "f3594820-3f90-4810-aa93-3e27fc747b2c"
)

func testAssetRecord() *cloudpbv2.Asset {
	return &cloudpbv2.Asset{Id: testAsset, OrganizationId: testTenant, Name: "esp32", PkiDeviceName: proto.String(testDevice)}
}

type cleanupConn struct {
	t             *testing.T
	asset         *cloudpbv2.Asset
	tokens        map[string]*pkicorepb.ListEnrollmentTokensResponse
	keys          map[string]*pkicorepb.ListEABKeysResponse
	mutations     []string
	fail          string
	changeBinding bool
	deleteSuccess bool
	getCount      int
}

func newCleanupConn(t *testing.T) *cleanupConn {
	return &cleanupConn{t: t, asset: testAssetRecord(), deleteSuccess: true,
		tokens: map[string]*pkicorepb.ListEnrollmentTokensResponse{
			"": {Tokens: []*pkicorepb.EnrollmentTokenInfo{
				{TokenId: firstToken, DeviceId: testDevice},
				{TokenId: "other-device", DeviceId: testDevice + "-other"},
				{TokenId: "case-mismatch", DeviceId: strings.ToUpper(testDevice)},
				{TokenId: "used", DeviceId: testDevice, Consumed: true},
				{TokenId: "revoked", DeviceId: testDevice, RevokedAt: "2026-01-01T00:00:00Z"},
				nil,
			}, NextPageToken: "next"},
			"next": {Tokens: []*pkicorepb.EnrollmentTokenInfo{{TokenId: secondToken, DeviceId: testDevice}}},
		},
		keys: map[string]*pkicorepb.ListEABKeysResponse{
			"": {Keys: []*pkicorepb.EABKeyInfo{
				{KeyId: "other-device", DeviceId: testDevice + "-other", Status: "active"},
				{KeyId: "used", DeviceId: testDevice, Status: "consumed"},
				{KeyId: "revoked", DeviceId: testDevice, Status: "revoked"},
				nil,
			}, NextPageToken: "next"},
			"next": {Keys: []*pkicorepb.EABKeyInfo{{KeyId: testEAB, DeviceId: testDevice, Status: "active"}}},
		},
	}
}

func (c *cleanupConn) decode(arg any, out proto.Message) {
	c.t.Helper()
	signed, ok := arg.(*cloudpbv2.SignedRequest)
	if !ok || len(signed.GetSignature()) == 0 || signed.GetPayloadType() != string(out.ProtoReflect().Descriptor().FullName()) {
		c.t.Fatalf("unsigned or wrongly typed mutation: %T", arg)
	}
	if err := proto.Unmarshal(signed.GetPayload(), out); err != nil {
		c.t.Fatal(err)
	}
}

func (c *cleanupConn) mutate(event string) error {
	c.mutations = append(c.mutations, event)
	if event == c.fail {
		return &platform.RPCStatusError{Code: platform.RPCUnavailable, Message: "injected failure"}
	}
	return nil
}

func (c *cleanupConn) Invoke(_ context.Context, method string, arg, reply any) error {
	switch method {
	case "/wendycloud.v2.OperatorSessionService/RegisterOperatorLeaf":
		return &platform.RPCStatusError{Code: platform.RPCUnimplemented, Message: "use x5c"}
	case "/wendycloud.v2.AssetService/GetAsset":
		if arg.(*cloudpbv2.GetAssetRequest).GetId() != testAsset {
			c.t.Fatal("read wrong asset")
		}
		c.getCount++
		asset := proto.Clone(c.asset).(*cloudpbv2.Asset)
		if c.changeBinding && c.getCount > 1 {
			asset.PkiDeviceName = proto.String("different-device")
		}
		proto.Merge(reply.(proto.Message), asset)
	case "/wendycloud.v2.PKIAdminService/ListEnrollmentTokens":
		req := arg.(*cloudpbv2.ListEnrollmentTokensRequest)
		if req.GetOrganizationId() != testTenant || !req.GetShowConsumed() {
			c.t.Fatal("incorrect token tenant or unstable token filter")
		}
		page, ok := c.tokens[req.GetPageToken()]
		if !ok {
			c.t.Fatalf("unexpected token page %q", req.GetPageToken())
		}
		proto.Merge(reply.(proto.Message), page)
	case "/wendycloud.v2.PKIAdminService/ListEABKeys":
		req := arg.(*cloudpbv2.ListEABKeysRequest)
		if req.GetOrganizationId() != testTenant || req.GetStatusFilter() != "" {
			c.t.Fatal("incorrect EAB tenant or unstable EAB filter")
		}
		page, ok := c.keys[req.GetPageToken()]
		if !ok {
			c.t.Fatalf("unexpected EAB page %q", req.GetPageToken())
		}
		proto.Merge(reply.(proto.Message), page)
	case "/wendycloud.v2.PKIAdminService/RevokeEnrollmentToken":
		var req cloudpbv2.RevokeEnrollmentTokenRequest
		c.decode(arg, &req)
		if req.GetOrganizationId() != testTenant {
			c.t.Fatal("revoked token in wrong tenant")
		}
		return c.mutate("token " + req.GetTokenId())
	case "/wendycloud.v2.PKIAdminService/RevokeEABKey":
		var req cloudpbv2.RevokeEABKeyRequest
		c.decode(arg, &req)
		if req.GetOrganizationId() != testTenant {
			c.t.Fatal("revoked EAB in wrong tenant")
		}
		return c.mutate("key " + req.GetKeyId())
	case "/wendycloud.v2.PKIAdminService/RevokePrincipalCertificates":
		var req cloudpbv2.RevokePrincipalCertificatesRequest
		c.decode(arg, &req)
		if req.GetOrganizationId() != testTenant || len(arg.(*cloudpbv2.SignedRequest).GetPkiManagementRequest()) == 0 {
			c.t.Fatal("missing tenant or PKI revocation signature")
		}
		if err := c.mutate("certificates"); err != nil {
			return err
		}
		proto.Merge(reply.(proto.Message), &pkicorepb.RevokeByPrincipalResponse{RevokedCount: 2})
	case "/wendycloud.v2.AssetService/DeleteAsset":
		var req cloudpbv2.DeleteAssetRequest
		c.decode(arg, &req)
		if req.GetId() != testAsset {
			c.t.Fatal("deleted wrong asset")
		}
		if err := c.mutate("delete"); err != nil {
			return err
		}
		proto.Merge(reply.(proto.Message), &cloudpbv2.DeleteAssetResponse{Success: c.deleteSuccess})
	default:
		c.t.Fatalf("unexpected RPC %s", method)
	}
	return nil
}

func TestRemoveScopesCredentialsAndRevokesBeforeDeleting(t *testing.T) {
	conn := newCleanupConn(t)
	result, err := testClient(t, conn).Remove(context.Background(), testSession, testAssetRecord())
	if err != nil || result.CredentialsRevoked != 3 || result.CertificatesRevoked != 2 || !result.AssetDeleted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	want := []string{"token " + firstToken, "token " + secondToken, "key " + testEAB, "certificates", "delete"}
	if !reflect.DeepEqual(conn.mutations, want) || conn.getCount < 2 {
		t.Fatalf("operations=%v reads=%d, want %v and binding recheck", conn.mutations, conn.getCount, want)
	}
}

// After cloud #873 the asset id is the device UUID and pki_device_name is empty;
// older and parked rows record the id their certificate names as pki_device_name.
func TestDeviceIDPrefersTheRecordedBindingThenTheAssetID(t *testing.T) {
	asset := testAssetRecord()
	asset.PkiDeviceName = nil
	if got := DeviceID(asset); got != testAsset {
		t.Fatalf("DeviceID = %q, want the asset id %q", got, testAsset)
	}
	if got := DeviceID(testAssetRecord()); got != testDevice {
		t.Fatalf("DeviceID = %q, want the recorded pki_device_name %q", got, testDevice)
	}
}

func TestRemoveStopsBeforeDeletionOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name, fail                   string
		changeBinding, deleteRefused bool
		credentials, certificates    int
		wantLast                     string
	}{
		{name: "token revocation", fail: "token " + firstToken, wantLast: "token " + firstToken},
		{name: "EAB revocation", fail: "key " + testEAB, credentials: 2, wantLast: "key " + testEAB},
		{name: "certificate revocation", fail: "certificates", credentials: 3, wantLast: "certificates"},
		{name: "binding changed", changeBinding: true, credentials: 3, certificates: 2, wantLast: "certificates"},
		{name: "deletion not confirmed", deleteRefused: true, credentials: 3, certificates: 2, wantLast: "delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := newCleanupConn(t)
			conn.fail, conn.changeBinding, conn.deleteSuccess = tc.fail, tc.changeBinding, !tc.deleteRefused
			result, err := testClient(t, conn).Remove(context.Background(), testSession, testAssetRecord())
			if err == nil || result.AssetDeleted || result.CredentialsRevoked != tc.credentials || result.CertificatesRevoked != tc.certificates {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if len(conn.mutations) == 0 || conn.mutations[len(conn.mutations)-1] != tc.wantLast {
				t.Fatalf("continued after failure: %v", conn.mutations)
			}
		})
	}
}

func TestRemoveRejectsPaginationCyclesBeforeMutation(t *testing.T) {
	for _, tokens := range []bool{true, false} {
		t.Run(fmt.Sprintf("tokens=%v", tokens), func(t *testing.T) {
			conn := newCleanupConn(t)
			if tokens {
				conn.tokens["next"].NextPageToken = "next"
			} else {
				conn.keys["next"].NextPageToken = "next"
			}
			result, err := testClient(t, conn).Remove(context.Background(), testSession, testAssetRecord())
			if err == nil || result.AssetDeleted || len(conn.mutations) != 0 {
				t.Fatalf("pagination cycle mutated state: result=%+v ops=%v err=%v", result, conn.mutations, err)
			}
		})
	}
}

func TestRemoveRejectsChangedTenantBeforeMutation(t *testing.T) {
	conn := newCleanupConn(t)
	conn.asset.OrganizationId = "7aa28a2c-3747-4dd5-aa38-c863128c1f9d"
	result, err := testClient(t, conn).Remove(context.Background(), testSession, testAssetRecord())
	if err == nil || result.AssetDeleted || len(conn.mutations) != 0 {
		t.Fatalf("cross-tenant cleanup: result=%+v ops=%v err=%v", result, conn.mutations, err)
	}
}

func (c *cleanupConn) Unary(ctx context.Context, r platform.RPCRequest) (platform.RPCResponse, error) {
	var request, reply proto.Message
	switch r.Method {
	case "/wendycloud.v2.AssetService/GetAsset":
		request = new(cloudpbv2.GetAssetRequest)
		reply = new(cloudpbv2.Asset)
	case "/wendycloud.v2.PKIAdminService/ListEnrollmentTokens":
		request = new(cloudpbv2.ListEnrollmentTokensRequest)
		reply = new(pkicorepb.ListEnrollmentTokensResponse)
	case "/wendycloud.v2.PKIAdminService/ListEABKeys":
		request = new(cloudpbv2.ListEABKeysRequest)
		reply = new(pkicorepb.ListEABKeysResponse)
	default:
		request = new(cloudpbv2.SignedRequest)
		switch r.Method {
		case "/wendycloud.v2.PKIAdminService/RevokeEnrollmentToken":
			reply = new(pkicorepb.RevokeEnrollmentTokenResponse)
		case "/wendycloud.v2.PKIAdminService/RevokeEABKey":
			reply = new(pkicorepb.RevokeEABKeyResponse)
		case "/wendycloud.v2.PKIAdminService/RevokePrincipalCertificates":
			reply = new(pkicorepb.RevokeByPrincipalResponse)
		case "/wendycloud.v2.AssetService/DeleteAsset":
			reply = new(cloudpbv2.DeleteAssetResponse)
		case "/wendycloud.v2.OperatorSessionService/RegisterOperatorLeaf":
			reply = new(cloudpbv2.RegisterOperatorLeafResponse)
		default:
			c.t.Fatalf("unexpected method %s", r.Method)
		}
	}
	if err := proto.Unmarshal(r.Message, request); err != nil {
		return platform.RPCResponse{}, err
	}
	if err := c.Invoke(ctx, r.Method, request, reply); err != nil {
		return platform.RPCResponse{}, err
	}
	wire, err := proto.Marshal(reply)
	return platform.RPCResponse{Message: wire}, err
}
func (*cleanupConn) ServerStream(context.Context, platform.RPCRequest) (platform.ServerStream, error) {
	panic("unexpected streaming call")
}
