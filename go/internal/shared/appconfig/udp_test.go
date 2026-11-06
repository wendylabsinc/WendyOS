package appconfig

import (
	"reflect"
	"testing"
)

func TestUDPPortMappingAnnotationAndValidation(t *testing.T) {
	ent := Entitlement{Type: EntitlementNetwork, Mode: "mesh", ServiceCIDR: "10.99.0.0/16", Ports: []PortMapping{
		{Host: 47777, Container: 7777, Protocol: "udp"},
		{Host: 47777, Container: 80},
	}}
	annotation := EntitlementAnnotationValue(ent)
	if got := ParseEntitlementAnnotation(EntitlementNetwork, annotation); !reflect.DeepEqual(got, ent) {
		t.Fatalf("UDP restart annotation %q: %+v", annotation, got)
	}
	for _, mode := range []string{"mesh", "bridge", "host", "none"} {
		candidate := ent
		candidate.Mode = mode
		if mode != "mesh" {
			candidate.ServiceCIDR = ""
		}
		err := validateEntitlements([]Entitlement{candidate}, "entitlements")
		if (err == nil) != (mode == "mesh") {
			t.Fatalf("UDP in mode %s: %v", mode, err)
		}
	}
	for _, port := range []PortMapping{{Host: 0, Container: 7777, Protocol: "udp"}, {Host: 7777, Container: 0, Protocol: "udp"}, {Host: 7777, Container: 7777, Protocol: "sctp"}} {
		candidate := ent
		candidate.Ports = []PortMapping{port}
		if err := validateEntitlements([]Entitlement{candidate}, "entitlements"); err == nil {
			t.Fatalf("accepted invalid mapping %+v", port)
		}
	}
}
