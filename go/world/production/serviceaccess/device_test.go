package serviceaccess

import (
	"encoding/json"
	"strings"
	"testing"
)

const selectorTenant = "11111111-1111-4111-8111-111111111111"
const selectorLocal = "spiffe://wendy.sh/tenant/" + selectorTenant + "/device/22222222-2222-4222-8222-222222222222"
const selectorRemote = "spiffe://wendy.sh/tenant/" + selectorTenant + "/device/33333333-3333-4333-8333-333333333333"

func TestDeviceSelectorsPreserveLocalEncodingAndSeparateOrigins(t *testing.T) {
	local := Source{App: "com.example.sensor", Service: "camera", ComponentType: "wendy.sensor.scalar.v1", Stream: "gas"}
	raw, _ := json.Marshal(local)
	if string(raw) != `{"app":"com.example.sensor","service":"camera","component_type":"wendy.sensor.scalar.v1","stream":"gas"}` {
		t.Fatal("omitted device changed the existing JSON contract")
	}
	legacyKey := `["com.example.sensor","camera","wendy.sensor.scalar.v1","gas","sample"]`
	if Key(local, "sample") != legacyKey {
		t.Fatal("local cursor bytes changed")
	}
	resolved, err := local.Resolve(selectorLocal)
	if err != nil || resolved.Device != selectorLocal || local.Device != "" {
		t.Fatal("local selector did not resolve without mutation", err)
	}
	remote := local
	remote.Device = selectorRemote
	if got, err := remote.Resolve(selectorLocal); err != nil || got != remote {
		t.Fatal("explicit origin was replaced by local identity", err)
	}
	keys := map[string]bool{}
	for _, s := range []Source{local, resolved, remote} {
		key := Key(s, "sample")
		if keys[key] {
			t.Fatal("different origins share a cursor key")
		}
		keys[key] = true
		if got, err := DecodeCursor(Cursor(key)); err != nil || got != key {
			t.Fatal("qualified cursor changed exact source", err)
		}
	}
	foreign := remote
	foreign.Device = strings.Replace(selectorRemote, selectorTenant, "44444444-4444-4444-8444-444444444444", 1)
	if foreign.Validate() != nil {
		t.Fatal("a selector syntax check must not infer local tenant authority")
	}
	if _, err := foreign.Resolve(selectorLocal); err == nil {
		t.Fatal("cross-tenant resolution accepted")
	}
	if _, err := DecodeCursor(Cursor(`["","com.example.sensor","camera","wendy.sensor.scalar.v1","gas","sample"]`)); err == nil {
		t.Fatal("noncanonical empty qualified device accepted")
	}
}

func TestDeviceSelectorsRefuseNoncanonicalPrincipals(t *testing.T) {
	for _, device := range []string{"*", "https://wendy.sh/tenant/" + selectorTenant, selectorRemote + "/", selectorRemote + "?x=1", strings.Replace(selectorRemote, "/device/", "/operator/", 1), strings.Replace(selectorRemote, selectorTenant, "{"+selectorTenant+"}", 1), strings.Replace(selectorRemote, "wendy.sh", "other.example", 1)} {
		s := Source{Device: device, App: "app", Service: "reader", ComponentType: "scalar", Stream: "gas"}
		if s.Validate() == nil {
			t.Fatal("noncanonical selector accepted", device)
		}
	}
}
