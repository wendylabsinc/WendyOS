package cloudrequest

import (
	"context"
	"crypto/mldsa"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type deletionCloud struct {
	*fakeCloud
	requests []*cloudpbv2.SignedRequest
}

func (f *deletionCloud) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	if method == cloudpbv2.AssetService_DeleteAsset_FullMethodName {
		f.requests = append(f.requests, proto.Clone(args.(*cloudpbv2.SignedRequest)).(*cloudpbv2.SignedRequest))
	}
	return f.fakeCloud.Invoke(ctx, method, args, reply, opts...)
}

// DeleteAsset is the only carrier of the revoke_principal authority Cloud
// relays to PKI (WDY-3939 #14). Lite identities are not UUIDs, and a retry
// after a refused kid needs a fresh PKI replay identity.
func TestDeleteAssetSignsExactLiteIdentityAndFreshRetry(t *testing.T) {
	const device = "lite-441bf6804824"
	auth, key, der := testAuth(t)
	conn := &deletionCloud{fakeCloud: &fakeCloud{t: t, refuse: []error{status.Error(codes.PermissionDenied, "kid unknown")}}}
	request := &cloudpbv2.DeleteAssetRequest{Id: "6fab25c6-01d3-4840-abcc-9d47259069a4", ExpectedDeviceId: proto.String(device)}
	if err := Invoke(context.Background(), conn, auth, cloudpbv2.AssetService_DeleteAsset_FullMethodName, request, &cloudpbv2.DeleteAssetResponse{}); err != nil || len(conn.requests) != 2 {
		t.Fatalf("deletion = %v, %d attempts", err, len(conn.requests))
	}
	seen := map[string]bool{}
	for _, signed := range conn.requests {
		var req cloudpbv2.DeleteAssetRequest
		if err := proto.Unmarshal(signed.Payload, &req); err != nil || !proto.Equal(&req, request) || signed.PayloadType != "wendycloud.v2.DeleteAssetRequest" {
			t.Fatalf("wrong request scope: %v, %v", &req, err)
		}
		parts := strings.Split(string(signed.PkiManagementRequest), ".")
		if len(parts) != 3 {
			t.Fatalf("missing management authority: %d segments", len(parts))
		}
		header, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			t.Fatal(err)
		}
		var h struct {
			Alg string   `json:"alg"`
			X5C []string `json:"x5c"`
		}
		if err := json.Unmarshal(header, &h); err != nil || h.Alg != "ML-DSA-65" || len(h.X5C) != 1 || h.X5C[0] != base64.StdEncoding.EncodeToString(der) {
			t.Fatalf("wrong management signer: %s, %v", header, err)
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		var claims struct {
			Op, Tenant, Principal, JTI string
			Iat, Exp                   int64
		}
		if err := json.Unmarshal(payload, &claims); err != nil {
			t.Fatal(err)
		}
		if claims.Op != "revoke_principal" || claims.Tenant != testTenant || claims.Principal != "spiffe://wendy.sh/tenant/"+testTenant+"/device/"+device || claims.Exp-claims.Iat != 30 {
			t.Fatalf("wrong management authority: %s", payload)
		}
		if _, err := uuid.Parse(claims.JTI); err != nil || seen[claims.JTI] {
			t.Fatalf("invalid or reused PKI replay identity %q", claims.JTI)
		}
		seen[claims.JTI] = true
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Fatal(err)
		}
		if err := mldsa.Verify(key.Public().(*mldsa.PublicKey), []byte(parts[0]+"."+parts[1]), signature, nil); err != nil {
			t.Fatalf("management signature: %v", err)
		}
	}
}

func TestDeleteAssetRejectsUnsafeDeviceBeforeRPC(t *testing.T) {
	auth, _, _ := testAuth(t)
	for _, device := range []string{"", "../other", "id?query", "id#fragment", "id%2fother", "id\n", strings.Repeat("a", 65)} {
		conn := &deletionCloud{fakeCloud: &fakeCloud{t: t}}
		request := &cloudpbv2.DeleteAssetRequest{Id: "6fab25c6-01d3-4840-abcc-9d47259069a4", ExpectedDeviceId: proto.String(device)}
		if err := Invoke(context.Background(), conn, auth, cloudpbv2.AssetService_DeleteAsset_FullMethodName, request, &cloudpbv2.DeleteAssetResponse{}); err == nil || len(conn.requests) != 0 {
			t.Errorf("device %q: err %v, deletions %d", device, err, len(conn.requests))
		}
	}
}

func TestUnenrollMutationSignaturesBindTenantAndAsset(t *testing.T) {
	auth, _, _ := testAuth(t)
	s, err := newSigner(auth)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, resource string
		request          proto.Message
	}{
		{cloudpbv2.AssetService_DeleteAsset_FullMethodName, "asset/6fab25c6-01d3-4840-abcc-9d47259069a4", &cloudpbv2.DeleteAssetRequest{Id: "6fab25c6-01d3-4840-abcc-9d47259069a4"}},
		{cloudpbv2.PKIAdminService_RevokeEnrollmentToken_FullMethodName, "org/" + testTenant + "/pki", &cloudpbv2.RevokeEnrollmentTokenRequest{OrganizationId: testTenant, TokenId: "token-1"}},
		{cloudpbv2.PKIAdminService_RevokeEABKey_FullMethodName, "org/" + testTenant + "/pki", &cloudpbv2.RevokeEABKeyRequest{OrganizationId: testTenant, KeyId: "eab-1"}},
	} {
		signed, err := s.signRequest(tc.method, tc.request, false, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		segments := strings.Split(string(signed.Signature), ".")
		payload, _ := base64.RawURLEncoding.DecodeString(segments[1])
		var descriptor struct {
			Target struct{ Resource, Tenant string }
		}
		if err := json.Unmarshal(payload, &descriptor); err != nil || descriptor.Target.Resource != tc.resource || descriptor.Target.Tenant != testTenant {
			t.Fatalf("descriptor %s, error %v", payload, err)
		}
	}
	for _, req := range []proto.Message{
		&cloudpbv2.RevokeEnrollmentTokenRequest{OrganizationId: uuid.NewString(), TokenId: "token-1"},
		&cloudpbv2.RevokeEnrollmentTokenRequest{OrganizationId: testTenant},
		&cloudpbv2.RevokeEABKeyRequest{OrganizationId: testTenant, KeyId: "eab-1"},
	} {
		if _, err := s.signRequest(cloudpbv2.PKIAdminService_RevokeEnrollmentToken_FullMethodName, req, false, uuid.NewString()); err == nil {
			t.Fatalf("signed mismatched mutation %v", req)
		}
	}
}
