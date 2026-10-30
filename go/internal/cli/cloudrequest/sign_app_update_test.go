package cloudrequest

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

func TestAppNotificationGrantSignatureBindsTenantAndCampaign(t *testing.T) {
	auth, _, _ := testAuth(t)
	signer, err := newSigner(auth)
	if err != nil {
		t.Fatal(err)
	}
	method := cloudpb.AppService_UpdateApp_FullMethodName
	request := &cloudpb.UpdateAppRequest{Id: "campaign:people", OrganizationId: testTenant, CanSendNotifications: proto.Bool(true)}
	signed, err := signer.signRequest(method, request, false, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(signed.GetSignature()), ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var descriptor struct {
		Operation string                            `json:"operation"`
		Target    struct{ Tenant, Resource string } `json:"target"`
	}
	if err := json.Unmarshal(raw, &descriptor); err != nil {
		t.Fatal(err)
	}
	if descriptor.Operation != strings.TrimPrefix(method, "/") || descriptor.Target.Tenant != testTenant || descriptor.Target.Resource != "org/"+testTenant+"/app/campaign:people" {
		t.Fatalf("wrong signed scope: %+v", descriptor)
	}
	for _, bad := range []proto.Message{&cloudpb.UpdateAppRequest{Id: "campaign:people", OrganizationId: "another-tenant"}, &cloudpb.UpdateAppRequest{OrganizationId: testTenant}, &cloudpb.UpsertAppRequest{Id: "campaign:people", OrganizationId: testTenant}} {
		if _, err := signer.signRequest(method, bad, false, uuid.NewString()); err == nil {
			t.Fatal("signed an invalid app grant target")
		}
	}
}
