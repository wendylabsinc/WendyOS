package cloudrequest

import (
	"context"
	"crypto/mldsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"github.com/wendylabsinc/wendy/go/proto/gen/pkicorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type principalCloud struct {
	*fakeCloud
	requests     []*cloudpbv2.SignedRequest
	responseWire []byte
}

func (f *principalCloud) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	if method == cloudpbv2.PKIAdminService_RevokePrincipalCertificates_FullMethodName {
		f.requests = append(f.requests, proto.Clone(args.(*cloudpbv2.SignedRequest)).(*cloudpbv2.SignedRequest))
		reply.(*pkicorepb.RevokeByPrincipalResponse).RevokedCount = 3
		if f.responseWire != nil {
			if err := proto.Unmarshal(f.responseWire, reply.(*pkicorepb.RevokeByPrincipalResponse)); err != nil {
				return err
			}
		}
	}
	return f.fakeCloud.Invoke(ctx, method, args, reply, opts...)
}

func TestPrincipalRevocationReadsOldAndAdditiveResponses(t *testing.T) {
	// The caller returns the existing count. Fencing annotations are server
	// metadata, not a new client-side success requirement.
	for _, tt := range []struct {
		name  string
		wire  string
		count int
	}{
		{"old", "0807", 7},
		{"with-fencing", "080910011a07626c6f636b6564", 9},
	} {
		t.Run(tt.name, func(t *testing.T) {
			auth, _, _ := testAuth(t)
			response, err := hex.DecodeString(tt.wire)
			if err != nil {
				t.Fatal(err)
			}
			conn := &principalCloud{fakeCloud: &fakeCloud{t: t}, responseWire: response}
			count, err := RevokePrincipalCertificates(context.Background(), conn, auth, "lite-441bf6804824")
			if err != nil || count != tt.count || len(conn.requests) != 1 {
				t.Fatalf("revocation = %d, %v, %d attempts", count, err, len(conn.requests))
			}
		})
	}
}

func TestPrincipalRevocationSignsExactLiteIdentityAndFreshRetry(t *testing.T) {
	auth, key, der := testAuth(t)
	conn := &principalCloud{fakeCloud: &fakeCloud{t: t, refuse: []error{status.Error(codes.PermissionDenied, "kid unknown")}}}
	count, err := RevokePrincipalCertificates(context.Background(), conn, auth, "lite-441bf6804824")
	if err != nil || count != 3 || len(conn.requests) != 2 {
		t.Fatalf("revocation = %d, %v, %d attempts", count, err, len(conn.requests))
	}
	seen := map[string]bool{}
	for _, signed := range conn.requests {
		var req cloudpbv2.RevokePrincipalCertificatesRequest
		if err := proto.Unmarshal(signed.Payload, &req); err != nil || req.GetOrganizationId() != testTenant || signed.PayloadType != "wendycloud.v2.RevokePrincipalCertificatesRequest" {
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
		if claims.Op != "revoke_principal" || claims.Tenant != testTenant || claims.Principal != "spiffe://wendy.sh/tenant/"+testTenant+"/device/lite-441bf6804824" || claims.Exp-claims.Iat != 30 {
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

func TestPrincipalRevocationRejectsInvalidDeviceBeforeRPC(t *testing.T) {
	auth, _, _ := testAuth(t)
	for _, device := range []string{"", "../other", "id?query", "id#fragment", "id%2fother", "id\n", strings.Repeat("a", 65)} {
		conn := &fakeCloud{t: t}
		if _, err := RevokePrincipalCertificates(context.Background(), conn, auth, device); err == nil || len(conn.calls) != 0 {
			t.Errorf("device %q: err %v, calls %v", device, err, conn.calls)
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
