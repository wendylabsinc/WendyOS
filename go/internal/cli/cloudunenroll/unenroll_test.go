package cloudunenroll

import (
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	pkicorepb "github.com/wendylabsinc/wendy/go/proto/gen/pkicorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	testTenant  = "2558fd76-afc7-466e-9613-6b715296a526"
	testAsset   = "6fab25c6-01d3-4840-abcc-9d47259069a4"
	testDevice  = "lite-441bf6804824"
	firstToken  = "ccf9d7eb-f72e-483a-a6d7-deed7169e322"
	secondToken = "793c38e2-986d-48b9-a368-d028f15da784"
	testEAB     = "f3594820-3f90-4810-aa93-3e27fc747b2c"
)

func testAuth(t *testing.T) *config.AuthConfig {
	t.Helper()
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	principal := "spiffe://wendy.sh/tenant/" + testTenant + "/operator/test"
	u, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, URIs: []*url.URL{u}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &config.AuthConfig{Certificates: []config.CertificateInfo{{PrincipalURI: principal, PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), PemPrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))}}}
}

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
	// member answers every PKIAdminService revocation the way Cloud answers a
	// caller with the default member role: device:delete, no pki:*-revoke.
	member bool
	// denyPKI answers every PKIAdminService call as Cloud does for a role
	// holding device:delete but no PKI administration permission.
	denyPKI bool
	deleted *cloudpbv2.SignedRequest
}

var errPKIDenied = status.Error(codes.PermissionDenied, `insufficient role in organization "Wendy Labs, Inc."`)

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
		return status.Error(codes.Unavailable, "injected failure")
	}
	return nil
}

func (c *cleanupConn) revoke(event string) error {
	if c.member {
		c.mutations = append(c.mutations, event)
		return status.Error(codes.PermissionDenied, "insufficient role in organization test")
	}
	return c.mutate(event)
}

func (c *cleanupConn) Invoke(_ context.Context, method string, arg, reply any, _ ...grpc.CallOption) error {
	if c.denyPKI && strings.HasPrefix(method, "/wendycloud.v2.PKIAdminService/") {
		c.mutations = append(c.mutations, "denied "+method)
		return errPKIDenied
	}
	switch method {
	case cloudpbv2.OperatorSessionService_RegisterOperatorLeaf_FullMethodName:
		return status.Error(codes.Unimplemented, "use x5c")
	case cloudpbv2.AssetService_GetAsset_FullMethodName:
		if arg.(*cloudpbv2.GetAssetRequest).GetId() != testAsset {
			c.t.Fatal("read wrong asset")
		}
		c.getCount++
		asset := proto.Clone(c.asset).(*cloudpbv2.Asset)
		if c.changeBinding && c.getCount > 1 {
			asset.PkiDeviceName = proto.String("different-device")
		}
		proto.Merge(reply.(proto.Message), asset)
	case cloudpbv2.PKIAdminService_ListEnrollmentTokens_FullMethodName:
		req := arg.(*cloudpbv2.ListEnrollmentTokensRequest)
		if req.GetOrganizationId() != testTenant || !req.GetShowConsumed() {
			c.t.Fatal("incorrect token tenant or unstable token filter")
		}
		page, ok := c.tokens[req.GetPageToken()]
		if !ok {
			c.t.Fatalf("unexpected token page %q", req.GetPageToken())
		}
		proto.Merge(reply.(proto.Message), page)
	case cloudpbv2.PKIAdminService_ListEABKeys_FullMethodName:
		req := arg.(*cloudpbv2.ListEABKeysRequest)
		if req.GetOrganizationId() != testTenant || req.GetStatusFilter() != "" {
			c.t.Fatal("incorrect EAB tenant or unstable EAB filter")
		}
		page, ok := c.keys[req.GetPageToken()]
		if !ok {
			c.t.Fatalf("unexpected EAB page %q", req.GetPageToken())
		}
		proto.Merge(reply.(proto.Message), page)
	case cloudpbv2.PKIAdminService_RevokeEnrollmentToken_FullMethodName:
		var req cloudpbv2.RevokeEnrollmentTokenRequest
		c.decode(arg, &req)
		if req.GetOrganizationId() != testTenant {
			c.t.Fatal("revoked token in wrong tenant")
		}
		return c.revoke("token " + req.GetTokenId())
	case cloudpbv2.PKIAdminService_RevokeEABKey_FullMethodName:
		var req cloudpbv2.RevokeEABKeyRequest
		c.decode(arg, &req)
		if req.GetOrganizationId() != testTenant {
			c.t.Fatal("revoked EAB in wrong tenant")
		}
		return c.revoke("key " + req.GetKeyId())
	case cloudpbv2.AssetService_DeleteAsset_FullMethodName:
		var req cloudpbv2.DeleteAssetRequest
		c.decode(arg, &req)
		if req.GetId() != testAsset || req.GetExpectedDeviceId() != DeviceID(c.asset) {
			c.t.Fatal("deleted wrong asset or without its device binding")
		}
		c.deleted = proto.Clone(arg.(*cloudpbv2.SignedRequest)).(*cloudpbv2.SignedRequest)
		if err := c.mutate("delete"); err != nil {
			return err
		}
		proto.Merge(reply.(proto.Message), &cloudpbv2.DeleteAssetResponse{Success: c.deleteSuccess})
	default:
		c.t.Fatalf("unexpected RPC %s", method)
	}
	return nil
}

func (c *cleanupConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, fmt.Errorf("unexpected streaming RPC")
}

func TestRemoveScopesCredentialsAndRevokesBeforeDeleting(t *testing.T) {
	conn := newCleanupConn(t)
	result, err := Remove(context.Background(), conn, testAuth(t), testAssetRecord())
	if err != nil || result.CredentialsRevoked != 3 || !result.AssetDeleted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	want := []string{"token " + firstToken, "token " + secondToken, "key " + testEAB, "delete"}
	if !reflect.DeepEqual(conn.mutations, want) || conn.getCount < 2 {
		t.Fatalf("operations=%v reads=%d, want %v and binding recheck", conn.mutations, conn.getCount, want)
	}
}

// A member holds device:delete but no pki:*-revoke right. Cloud's DeleteAsset
// revokes the device principal under device:delete, so even a parked device
// whose enrollment completed is removed by DeleteAsset alone (WDY-3939 #14).
func TestRemoveByMemberDeletesWithoutCertificateRevocation(t *testing.T) {
	conn := newCleanupConn(t)
	conn.member = true
	conn.tokens = map[string]*pkicorepb.ListEnrollmentTokensResponse{"": {Tokens: []*pkicorepb.EnrollmentTokenInfo{
		{TokenId: "used", DeviceId: testDevice, Consumed: true},
		{TokenId: "other-device", DeviceId: testDevice + "-other"},
	}}}
	conn.keys = map[string]*pkicorepb.ListEABKeysResponse{"": {Keys: []*pkicorepb.EABKeyInfo{
		{KeyId: "used", DeviceId: testDevice, Status: "consumed"},
		{KeyId: "revoked", DeviceId: testDevice, Status: "revoked"},
	}}}
	result, err := Remove(context.Background(), conn, testAuth(t), testAssetRecord())
	if err != nil || !result.AssetDeleted || result.CredentialsRevoked != 0 {
		t.Fatalf("member unenrollment: result=%+v err=%v", result, err)
	}
	if !reflect.DeepEqual(conn.mutations, []string{"delete"}) {
		t.Fatalf("operations=%v, want DeleteAsset alone", conn.mutations)
	}
}

// Cloud revokes inside DeleteAsset only with the operator's own authority: a
// revoke_principal request for exactly this device, signed by the same leaf.
func TestRemoveDeletionCarriesSameLeafPrincipalRevocation(t *testing.T) {
	conn := newCleanupConn(t)
	if _, err := Remove(context.Background(), conn, testAuth(t), testAssetRecord()); err != nil {
		t.Fatal(err)
	}
	if conn.deleted == nil {
		t.Fatal("DeleteAsset was not sent")
	}
	requestLeaf, _ := decodeJWS(t, conn.deleted.GetSignature())
	managementLeaf, payload := decodeJWS(t, conn.deleted.GetPkiManagementRequest())
	var claims struct {
		Op, Tenant, Principal string
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Op != "revoke_principal" || claims.Tenant != testTenant || claims.Principal != "spiffe://wendy.sh/tenant/"+testTenant+"/device/"+testDevice {
		t.Fatalf("wrong revocation authority: %s", payload)
	}
	if managementLeaf != requestLeaf {
		t.Fatal("revocation authority signed by a different leaf than the request")
	}
}

// decodeJWS returns the leaf certificate a compact JWS names in its x5c header,
// and its decoded payload.
func decodeJWS(t *testing.T, jws []byte) (string, []byte) {
	t.Helper()
	parts := strings.Split(string(jws), ".")
	if len(parts) != 3 {
		t.Fatalf("malformed JWS with %d segments", len(parts))
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var h struct {
		X5C []string `json:"x5c"`
	}
	if err := json.Unmarshal(header, &h); err != nil || len(h.X5C) == 0 {
		t.Fatalf("JWS does not name its leaf: %s %v", header, err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	return h.X5C[0], payload
}

// Revoking an unused enrollment credential stays owner/admin-only, so a member
// stops before DeleteAsset and is told who can finish.
func TestRemoveByMemberStopsAtUnusedCredentialAndNamesAdmin(t *testing.T) {
	for _, tc := range []struct {
		name, kind, wantOp string
		noTokens           bool
	}{
		{name: "enrollment token", kind: "enrollment token", wantOp: "token " + firstToken},
		{name: "EAB credential", kind: "EAB credential", wantOp: "key " + testEAB, noTokens: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := newCleanupConn(t)
			conn.member = true
			if tc.noTokens {
				conn.tokens = map[string]*pkicorepb.ListEnrollmentTokensResponse{"": {}}
			}
			result, err := Remove(context.Background(), conn, testAuth(t), testAssetRecord())
			if err == nil || result.AssetDeleted || result.CredentialsRevoked != 0 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "unused "+tc.kind) || !strings.Contains(err.Error(), "owner or admin") {
				t.Fatalf("refusal does not explain the admin-only step: %v", err)
			}
			if !reflect.DeepEqual(conn.mutations, []string{tc.wantOp}) {
				t.Fatalf("operations=%v, want only %q", conn.mutations, tc.wantOp)
			}
		})
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
		credentials                  int
		wantLast                     string
	}{
		{name: "token revocation", fail: "token " + firstToken, wantLast: "token " + firstToken},
		{name: "EAB revocation", fail: "key " + testEAB, credentials: 2, wantLast: "key " + testEAB},
		{name: "binding changed", changeBinding: true, credentials: 3, wantLast: "key " + testEAB},
		{name: "deletion not confirmed", deleteRefused: true, credentials: 3, wantLast: "delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := newCleanupConn(t)
			conn.fail, conn.changeBinding, conn.deleteSuccess = tc.fail, tc.changeBinding, !tc.deleteRefused
			result, err := Remove(context.Background(), conn, testAuth(t), testAssetRecord())
			if err == nil || result.AssetDeleted || result.CredentialsRevoked != tc.credentials {
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
			result, err := Remove(context.Background(), conn, testAuth(t), testAssetRecord())
			if err == nil || result.AssetDeleted || len(conn.mutations) != 0 {
				t.Fatalf("pagination cycle mutated state: result=%+v ops=%v err=%v", result, conn.mutations, err)
			}
		})
	}
}

func TestRemoveRejectsChangedTenantBeforeMutation(t *testing.T) {
	conn := newCleanupConn(t)
	conn.asset.OrganizationId = "7aa28a2c-3747-4dd5-aa38-c863128c1f9d"
	result, err := Remove(context.Background(), conn, testAuth(t), testAssetRecord())
	if err == nil || result.AssetDeleted || len(conn.mutations) != 0 {
		t.Fatalf("cross-tenant cleanup: result=%+v ops=%v err=%v", result, conn.mutations, err)
	}
}

// A device enrolled under its UUID has asset id == device id. DeleteAsset then
// has pki-core fence the enrollment under device:delete: it revokes the
// certificates, enrollment tokens and EAB keys in one transaction, and Cloud
// refuses to delete unless pki-core confirms. Removal must not first ask for
// PKI administration permissions the caller may lack. On 2026-10-26 a Lite
// unenroll stopped here with PermissionDenied listing enrollment tokens.
func TestRemoveLeavesFencedCleanupToDeleteAsset(t *testing.T) {
	for _, name := range []*string{proto.String(testAsset), nil} {
		t.Run(fmt.Sprintf("pki_device_name=%v", name != nil), func(t *testing.T) {
			conn := newCleanupConn(t)
			conn.asset.PkiDeviceName, conn.denyPKI = name, true
			result, err := Remove(context.Background(), conn, testAuth(t), proto.Clone(conn.asset).(*cloudpbv2.Asset))
			if err != nil || !result.AssetDeleted || !result.EnrollmentFenced || result.CredentialsRevoked != 0 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if !reflect.DeepEqual(conn.mutations, []string{"delete"}) {
				t.Fatalf("operations=%v, want only the deletion", conn.mutations)
			}
			// The fence rides on this deletion: the exact device guard, and the
			// same operator's revoke_principal request for this device.
			var req cloudpbv2.DeleteAssetRequest
			conn.decode(conn.deleted, &req)
			_, payload := decodeJWS(t, conn.deleted.GetPkiManagementRequest())
			var claims struct{ Op, Principal string }
			if err := json.Unmarshal(payload, &claims); err != nil {
				t.Fatal(err)
			}
			if req.GetExpectedDeviceId() != testAsset || claims.Op != "revoke_principal" || claims.Principal != "spiffe://wendy.sh/tenant/"+testTenant+"/device/"+testAsset {
				t.Fatalf("deletion guards %q and revokes %+v, want this device", req.GetExpectedDeviceId(), claims)
			}
		})
	}
}

// A row the re-key parked keeps a former name that Cloud cannot fence, so its
// unused credentials are still revoked here first. A caller who may not list
// them is refused before anything changes; the deletion is never sent.
func TestRemoveParkedAssetFailsClosedWithoutCredentialPermissions(t *testing.T) {
	conn := newCleanupConn(t)
	conn.denyPKI = true
	result, err := Remove(context.Background(), conn, testAuth(t), testAssetRecord())
	if status.Code(err) != codes.PermissionDenied || result.AssetDeleted || result.EnrollmentFenced || conn.deleted != nil {
		t.Fatalf("result=%+v operations=%v err=%v", result, conn.mutations, err)
	}
}
