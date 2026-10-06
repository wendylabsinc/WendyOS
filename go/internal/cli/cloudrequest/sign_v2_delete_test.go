package cloudrequest

import (
	"context"
	"crypto/mldsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

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
	req := &cloudpbv2.DeleteAssetRequest{Id: id}
	signed, err := signer.signRequest(method, req, false, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	var decoded cloudpbv2.DeleteAssetRequest
	if signed.GetPayloadType() != "wendycloud.v2.DeleteAssetRequest" || proto.Unmarshal(signed.GetPayload(), &decoded) != nil || !proto.Equal(req, &decoded) {
		t.Fatal("deletion envelope lost its exact UUID/type")
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
