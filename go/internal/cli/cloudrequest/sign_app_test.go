package cloudrequest

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

func TestAppRegistrationSignatureRequiresMatchingTenantAndPayload(t *testing.T) {
	auth, _, _ := testAuth(t)
	signer, err := newSigner(auth)
	if err != nil {
		t.Fatal(err)
	}
	method := cloudpb.AppService_UpsertApp_FullMethodName
	for _, req := range []proto.Message{
		&cloudpb.UpsertAppRequest{OrganizationId: "11111111-1111-4111-8111-111111111111", Id: "app"},
		&cloudpb.UpsertAppRequest{OrganizationId: testTenant},
		&cloudpb.GetAppRequest{OrganizationId: testTenant, Id: "app"},
	} {
		if _, err := signer.signRequest(method, req, false, "test-flow"); err == nil {
			t.Fatalf("signed mismatched tenant, empty ID, or incorrect request type: %v", req)
		}
	}
	signed, err := signer.signRequest(method, &cloudpb.UpsertAppRequest{OrganizationId: testTenant, Id: "campaign:people"}, false, "test-flow")
	if err != nil {
		t.Fatal(err)
	}
	if signed.GetPayloadType() != "wendycloud.v2.UpsertAppRequest" {
		t.Fatalf("wrong payload type: %s", signed.GetPayloadType())
	}
	parts := strings.Split(string(signed.GetSignature()), ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Operation string                            `json:"operation"`
		Target    struct{ Tenant, Resource string } `json:"target"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Operation != "wendycloud.v2.AppService/UpsertApp" || claims.Target.Tenant != testTenant || claims.Target.Resource != "org/"+testTenant+"/app/campaign:people" {
		t.Fatalf("wrong app signature target: %s (%v)", raw, err)
	}
}
