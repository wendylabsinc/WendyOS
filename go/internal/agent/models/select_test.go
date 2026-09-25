package models

import (
	"strings"
	"testing"
)

func TestSelectVariantFollowsCatalogOrder(t *testing.T) {
	c, err := ParseCatalog([]byte(validCatalogJSON()))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := c.Model("coco-detector")
	cases := []struct {
		name string
		dev  DeviceProfile
		want string
	}{
		{"qualcomm", DeviceProfile{Arch: "arm64", GPUVendor: "qualcomm", NPUBackends: []string{"qnn"}}, "d-qnn"},
		{"jetson", DeviceProfile{Arch: "arm64", GPUVendor: "nvidia", ComputeBackends: []string{"cuda"}}, "d-trt"},
		{"raspberry pi 5", DeviceProfile{Arch: "arm64", GPUVendor: "broadcom"}, "d-cpu"},
	}
	for _, tc := range cases {
		v, reason, ok := SelectVariant(m, tc.dev)
		if !ok || v.ID != tc.want {
			t.Errorf("%s: got %q (%v, %q), want %q", tc.name, v.ID, ok, reason, tc.want)
		}
	}
}

func TestSelectVariantExplainsMismatch(t *testing.T) {
	c, _ := ParseCatalog([]byte(validCatalogJSON()))
	m, _ := c.Model("coco-detector")
	_, reason, ok := SelectVariant(m, DeviceProfile{Arch: "amd64"})
	if ok {
		t.Fatal("an amd64 CPU-only device matched")
	}
	for _, want := range []string{"d-qnn needs NPU backend qnn", "d-trt needs a nvidia GPU", "d-cpu needs arch arm64"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q lacks %q", reason, want)
		}
	}
}
