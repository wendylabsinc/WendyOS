package cloudrequest

import (
	"context"
	"crypto/mldsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

func TestV2DeleteAssetRequiresSignedCanonicalUUID(t *testing.T) {
	const method = cloudpbv2.AssetService_DeleteAsset_FullMethodName
	const id = "22222222-2222-4222-8222-222222222222"
	auth, key, _ := testAuth(t)
	signer, err := newSigner(auth)
	if err != nil {
		t.Fatal(err)
	}
	req := &cloudpbv2.DeleteAssetRequest{Id: id, ExpectedDeviceId: proto.String("33333333-3333-4333-8333-333333333333")}
	signed, err := signer.signRequest(method, req, false, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	var decoded cloudpbv2.DeleteAssetRequest
	if signed.GetPayloadType() != "wendycloud.v2.DeleteAssetRequest" || proto.Unmarshal(signed.GetPayload(), &decoded) != nil || !proto.Equal(req, &decoded) {
		t.Fatal("deletion envelope lost its exact UUID/type/expected PKI binding")
	}
	parts := strings.Split(string(signed.GetSignature()), ".")
	if len(parts) != 3 {
		t.Fatal("missing deletion signature")
	}
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var descriptor struct {
		Operation  string `json:"operation"`
		BodyDigest string `json:"body_sha256"`
		Target     struct {
			Resource string `json:"resource"`
			Tenant   string `json:"tenant"`
		} `json:"target"`
	}
	if err := json.Unmarshal(claims, &descriptor); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(signed.GetPayload())
	if descriptor.Operation != strings.TrimPrefix(method, "/") || descriptor.Target.Resource != "asset/"+id || descriptor.Target.Tenant != testTenant || descriptor.BodyDigest != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatalf("incorrect signed deletion descriptor: %+v", descriptor)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := mldsa.Verify(key.Public().(*mldsa.PublicKey), []byte(parts[0]+"."+parts[1]), sig, nil); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "123", "asset/" + id, "../../other", "22222222222242228222222222222222", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"} {
		if _, err := signer.signRequest(method, &cloudpbv2.DeleteAssetRequest{Id: bad}, false, uuid.NewString()); err == nil {
			t.Fatalf("invalid deletion signed: %q", bad)
		}
	}
	if _, err := signer.signRequest(method, &cloudpbv2.GetAssetRequest{Id: id}, false, uuid.NewString()); err == nil {
		t.Fatal("wrong request type accepted")
	}
	// The new deletion method participates in main's supported kid/x5c flow.
	conn := &fakeCloud{t: t}
	if err := Invoke(context.Background(), conn, auth, method, req, &cloudpbv2.DeleteAssetResponse{}); err != nil {
		t.Fatal(err)
	}
	want := cloudpbv2.OperatorSessionService_RegisterOperatorLeaf_FullMethodName + " x5c\n" + method + " kid"
	if strings.Join(conn.calls, "\n") != want {
		t.Fatalf("calls %v, want %s", conn.calls, want)
	}
}

func TestV2DeleteCarriesSameOperatorExactPrincipalAuthority(t *testing.T) {
	auth, key, _ := testAuth(t)
	signer, err := newSigner(auth)
	if err != nil {
		t.Fatal(err)
	}
	signer.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	const device = "33333333-3333-4333-8333-333333333333"
	request := &cloudpbv2.DeleteAssetRequest{Id: "22222222-2222-4222-8222-222222222222", ExpectedDeviceId: proto.String(device)}
	seen := make(map[string]bool)
	for _, byKID := range []bool{false, true, true} {
		signed, err := signer.signRequest(cloudpbv2.AssetService_DeleteAsset_FullMethodName, request, byKID, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(string(signed.GetPkiManagementRequest()), ".")
		if len(parts) != 3 {
			t.Fatal("missing PKI management authority")
		}
		headerBytes, _ := base64.RawURLEncoding.DecodeString(parts[0])
		var header struct {
			Algorithm string   `json:"alg"`
			X5C       []string `json:"x5c"`
		}
		if err := json.Unmarshal(headerBytes, &header); err != nil {
			t.Fatal(err)
		}
		if header.Algorithm != "ML-DSA-65" || len(header.X5C) != len(signer.x5c) || strings.Join(header.X5C, ",") != strings.Join(signer.x5c, ",") {
			t.Fatal("management request must carry the same operator chain")
		}
		payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims struct {
			Op        string `json:"op"`
			Tenant    string `json:"tenant"`
			Principal string `json:"principal"`
			Iat       int64  `json:"iat"`
			Exp       int64  `json:"exp"`
			JTI       string `json:"jti"`
		}
		if err := json.Unmarshal(payload, &claims); err != nil {
			t.Fatal(err)
		}
		if claims.Op != "revoke_principal" || claims.Tenant != testTenant || claims.Principal != "spiffe://wendy.sh/tenant/"+testTenant+"/device/"+device || claims.Iat != signer.now().Unix() || claims.Exp != claims.Iat+30 {
			t.Fatalf("wrong management authority: %+v", claims)
		}
		if _, err := uuid.Parse(claims.JTI); err != nil || seen[claims.JTI] {
			t.Fatal("management request must have a fresh canonical replay identity")
		}
		seen[claims.JTI] = true
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		if err := mldsa.Verify(key.Public().(*mldsa.PublicKey), []byte(parts[0]+"."+parts[1]), sig, nil); err != nil {
			t.Fatal(err)
		}
	}
	request.ExpectedDeviceId = nil
	plain, err := signer.signRequest(cloudpbv2.AssetService_DeleteAsset_FullMethodName, request, false, uuid.NewString())
	if err != nil || len(plain.GetPkiManagementRequest()) != 0 {
		t.Fatal("ordinary unbound asset deletion must not gain revocation authority")
	}
	for _, invalid := range []string{"", "../other", "spiffe://wendy.sh/tenant/other/device/other", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"} {
		request.ExpectedDeviceId = proto.String(invalid)
		if _, err := signer.signRequest(cloudpbv2.AssetService_DeleteAsset_FullMethodName, request, false, uuid.NewString()); err == nil {
			t.Fatalf("invalid device identity signed: %q", invalid)
		}
	}
}
