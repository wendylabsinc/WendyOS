package cloudrequest

import (
	"testing"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

// The registry methods sign the resources cloud's RequestSigning.md names, and
// refuse a request for another tenant or a device that is not the tenant's
// canonical device URI; the assignments are x5c-only and postbox-relayed.
func TestRegistryResources(t *testing.T) {
	const (
		tenant  = "2558fd76-afc7-466e-9613-6b715296a526"
		release = "0b5c1f3e-2a4b-4c6d-8e9f-0a1b2c3d4e5f"
		deploy  = "1c6d2e4f-3b5a-4d7e-9fa0-1b2c3d4e5f60"
		device  = "7c1d3e5f-2a4b-4c6d-8e9f-0a1b2c3d4e5f"
	)
	uri := "spiffe://wendy.sh/tenant/" + tenant + "/device/" + device
	cases := []struct {
		method string
		req    proto.Message
		want   string // empty: refused
	}{
		{cloudpbv2.DeploymentService_CreateDeployment_FullMethodName, &cloudpbv2.CreateDeploymentRequest{OrganizationId: tenant, AppReleaseId: release}, "org/" + tenant + "/release/" + release},
		{cloudpbv2.ArtifactRegistryService_PublishArtifact_FullMethodName, &cloudpbv2.PublishArtifactRequest{OrganizationId: tenant, AppReleaseId: release}, "org/" + tenant + "/release/" + release},
		{cloudpbv2.ArtifactRegistryService_PublishArtifact_FullMethodName, &cloudpbv2.PublishArtifactRequest{OrganizationId: tenant, AppReleaseId: "0B5C1F3E-2A4B-4C6D-8E9F-0A1B2C3D4E5F"}, ""},
		{cloudpbv2.ArtifactRegistryService_AddWraps_FullMethodName, &cloudpbv2.AddWrapsRequest{OrganizationId: tenant}, "org/" + tenant + "/wraps"},
		{cloudpbv2.ArtifactRegistryService_AddWraps_FullMethodName, &cloudpbv2.AddWrapsRequest{OrganizationId: release}, ""},
		{cloudpbv2.ArtifactRegistryService_AssignDevice_FullMethodName, &cloudpbv2.DeviceAssignment{OrganizationId: tenant, DeploymentId: deploy, Device: uri}, "org/" + tenant + "/deployment/" + deploy + "/device/" + device},
		{cloudpbv2.ArtifactRegistryService_AssignDevice_FullMethodName, &cloudpbv2.DeviceAssignment{OrganizationId: tenant, DeploymentId: deploy, Device: "spiffe://wendy.sh/tenant/" + release + "/device/" + device}, ""},
		{cloudpbv2.ArtifactRegistryService_AssignDevice_FullMethodName, &cloudpbv2.DeviceAssignment{OrganizationId: tenant, DeploymentId: deploy, Device: uri + "/x"}, ""},
		{cloudpbv2.ArtifactRegistryService_UnassignDevice_FullMethodName, &cloudpbv2.UnassignDeviceRequest{OrganizationId: tenant, DeploymentId: deploy, Device: uri}, "org/" + tenant + "/deployment/" + deploy + "/device/" + device},
		{cloudpbv2.ArtifactRegistryService_UnassignDevice_FullMethodName, &cloudpbv2.UnassignDeviceRequest{OrganizationId: tenant, DeploymentId: "", Device: uri}, ""},
		{cloudpbv2.ArtifactRegistryService_UnassignDevice_FullMethodName, &cloudpbv2.DeviceAssignment{OrganizationId: tenant, DeploymentId: deploy, Device: uri}, ""},
		{cloudpbv2.DeploymentService_DetachDevice_FullMethodName, &cloudpbv2.DetachDeviceRequest{OrganizationId: tenant, DeploymentId: deploy, Device: uri}, "org/" + tenant + "/deployment/" + deploy + "/device/" + device},
		{cloudpbv2.DeploymentService_DetachDevice_FullMethodName, &cloudpbv2.DetachDeviceRequest{OrganizationId: release, DeploymentId: deploy, Device: uri}, ""},
		{cloudpbv2.AppService_DeleteApp_FullMethodName, &cloudpbv2.DeleteAppRequest{OrganizationId: tenant, Id: "sh.wendy.example"}, "org/" + tenant + "/app/sh.wendy.example"},
		{cloudpbv2.AppService_DeleteApp_FullMethodName, &cloudpbv2.DeleteAppRequest{OrganizationId: release, Id: "sh.wendy.example"}, ""},
	}
	for _, c := range cases {
		got, ok := signedResources[c.method](tenant, c.req)
		if ok != (c.want != "") || ok && got != c.want {
			t.Errorf("%s %v: got %q %t, want %q", c.method, c.req, got, ok, c.want)
		}
	}
	for _, m := range []string{cloudpbv2.ArtifactRegistryService_AssignDevice_FullMethodName, cloudpbv2.ArtifactRegistryService_UnassignDevice_FullMethodName, cloudpbv2.DeploymentService_DetachDevice_FullMethodName} {
		if !x5cOnly[m] || !relayedSignatures[m] {
			t.Errorf("%s must be x5c-only and postbox-relayed", m)
		}
	}
}
