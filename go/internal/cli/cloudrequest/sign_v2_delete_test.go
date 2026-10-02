package cloudrequest

import (
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"testing"
)

func TestV2DeleteAssetRequiresSignedCanonicalUUID(t *testing.T) {
	const method = "/wendycloud.v2.AssetService/DeleteAsset"
	const id = "22222222-2222-4222-8222-222222222222"
	resource, required, err := signedResource(method, &cloudpbv2.DeleteAssetRequest{Id: id})
	if err != nil || !required || resource != "asset/"+id {
		t.Fatalf("signature target %q %v %v", resource, required, err)
	}
	for _, bad := range []string{"", "123", "asset/" + id, "../../other", "22222222222242228222222222222222"} {
		if _, required, err := signedResource(method, &cloudpbv2.DeleteAssetRequest{Id: bad}); !required || err == nil {
			t.Fatalf("unsigned/invalid deletion %q", bad)
		}
	}
	if _, required, err := signedResource(method, &cloudpbv2.GetAssetRequest{Id: id}); !required || err == nil {
		t.Fatal("wrong request type accepted")
	}
}
