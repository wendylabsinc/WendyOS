package mcp

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func TestDeviceListV2ReturnsReusableSelector(t *testing.T) {
	const deviceID = "11111111-1111-4111-8111-111111111111"
	addr := startFakeAssetV2Server(t, &fakeAssetV2Server{online: []*cloudpbv2.Asset{{Id: deviceID, Name: "edge-one", IsComputeDevice: true}}})
	auth := v2TestAuth(t, addr)
	s := New(&config.Config{Auth: []config.AuthConfig{*auth}}, nil)
	want := "cloud://" + addr + "/tenant/" + auth.Certificates[0].TenantUUID() + "/asset/" + deviceID
	for _, name := range []string{"device_list", "cloud_discover"} {
		r, err := s.callTool(context.Background(), name, nil)
		if err != nil || r.IsError {
			t.Fatalf("%s: %v %v", name, r, err)
		}
		rows := listPayload(t, r, "devices")
		if len(rows) != 1 || rows[0]["device"] != want || rows[0]["device_id"] != deviceID {
			t.Fatalf("lost v2 identity in %s: %v", name, rows)
		}
	}
}

// cloudAssetV2ToMap must emit the device id (the v2 asset UUID) as device_id,
// with device_name — never an int32 asset id (sem's "device ID, not asset ID"
// rule, WDY-3146).
func TestCloudAssetV2ToMapEmitsDeviceIDNotAssetID(t *testing.T) {
	dt := "jetson-orin-nano"
	a := &cloudpbv2.Asset{
		Id:              "00000000-0000-4000-8000-000000000042",
		Name:            "spark-01",
		OrganizationId:  "39752b06-dd09-40f2-9a07-83065cfb5f05",
		IsComputeDevice: true,
		DeviceType:      &dt,
	}
	m := cloudAssetV2ToMap(a)

	if m["device_id"] != a.GetId() {
		t.Errorf("device_id = %v, want the asset UUID %q", m["device_id"], a.GetId())
	}
	if m["device_name"] != a.GetName() {
		t.Errorf("device_name = %v, want %q", m["device_name"], a.GetName())
	}
	// The v1-era int32 identity keys must not leak into the v2 output.
	for _, k := range []string{"id", "asset_id"} {
		if _, ok := m[k]; ok {
			t.Errorf("v2 device map must not carry %q (device ID, not asset ID)", k)
		}
	}
	if m["device_type"] != dt {
		t.Errorf("device_type = %v, want %q", m["device_type"], dt)
	}
}
