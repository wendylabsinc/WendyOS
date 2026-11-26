//go:build e2e_apps_fixture

package cloudrequest

import (
	"strings"

	"github.com/google/uuid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

// Test-driver-only mappings for ordinary, authorized Site fixture operations.
// Normal CLI/Agent builds do not compile this file. Keep the existing signer,
// token transport, validity stamping, leaf registration and Cloud verification.
// Resource shapes match Cloud SiteServiceHandler createSite/deleteSite.
func init() {
	signedResources[pb.AssetService_CreateAsset_FullMethodName] = func(tenant string, request proto.Message) (string, bool) {
		in, ok := request.(*pb.CreateAssetRequest)
		if !ok || tenant == "" || in.OrganizationId != tenant ||
			!strings.HasPrefix(in.Details, "sh.wendy.e2e.apps-") || in.Name != strings.ReplaceAll(in.Details, ".", "-")+"-unbound" ||
			in.GetPkiDeviceName() != "" || in.GetParentAssetId() != "" || !in.IsComputeDevice {
			return "", false
		}
		// Ordinary same-tenant catalog declaration, without enrollment or a
		// device identity. Matches AssetServiceHandler.createAsset's resource.
		return "org/" + tenant + "/asset-name/" + in.Name, true
	}
	signedResources[pb.SiteService_CreateSite_FullMethodName] = func(tenant string, request proto.Message) (string, bool) {
		in, ok := request.(*pb.CreateSiteRequest)
		if !ok || tenant == "" || in.OrganizationId != tenant || !strings.HasPrefix(in.Name, "sh.wendy.e2e.apps-") || len(in.Name) > 64 {
			return "", false
		}
		return "org/" + tenant + "/site/" + in.Name, true
	}
	signedResources[pb.SiteService_DeleteSite_FullMethodName] = func(tenant string, request proto.Message) (string, bool) {
		in, ok := request.(*pb.DeleteSiteRequest)
		if !ok || tenant == "" || in.OrganizationId != tenant {
			return "", false
		}
		id, err := uuid.Parse(in.SiteId)
		if err != nil || id.String() != in.SiteId {
			return "", false
		}
		return "org/" + tenant + "/site/" + in.SiteId, true
	}
}
