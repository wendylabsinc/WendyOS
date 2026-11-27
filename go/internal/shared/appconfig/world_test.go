package appconfig

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestWorldPublicationAnnotationRoundTrip(t *testing.T) {
	want := Entitlement{Type: EntitlementWorld, WorldPublish: []WorldPublication{{ComponentType: "wendy.pose.v1", Stream: "north/people,raw=x"}}}
	encoded := EntitlementAnnotationValue(want)
	got := ParseEntitlementAnnotation(EntitlementWorld, encoded)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: %+v", got)
	}
	for _, bad := range []string{encoded + ",publish=e30", "present=true", "publish=not base64", strings.Repeat("x", 4097)} {
		if ValidateWorldPublications(ParseEntitlementAnnotation(EntitlementWorld, bad).WorldPublish) == nil {
			t.Fatalf("accepted invalid annotation %q", bad)
		}
	}
}

func TestWorldReadOnlyAndMixedAccessRoundTrip(t *testing.T) {
	source := WorldSubscription{App: "com.example.camera", Service: "front", ComponentType: "wendy.sensor.capture.v1", Stream: "camera-a"}
	for _, publish := range [][]WorldPublication{nil, {{ComponentType: "wendy.inference.claim.v1", Stream: "hazard"}}} {
		want := Entitlement{Type: EntitlementWorld, WorldPublish: publish, WorldSubscribe: []WorldSubscription{source}}
		encoded := EntitlementAnnotationValue(want)
		if !strings.HasPrefix(encoded, "access=") || len(encoded)+len(EntitlementAnnotationKeyPrefix+EntitlementWorld) > 4096 {
			t.Fatal("invalid persisted access encoding")
		}
		got := ParseEntitlementAnnotation(EntitlementWorld, encoded)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("lost exact subscription: %+v", got)
		}
		pub, sub, found, err := WorldAccess([]Entitlement{want})
		if err != nil || !found || !reflect.DeepEqual(pub, publish) || !reflect.DeepEqual(sub, want.WorldSubscribe) {
			t.Fatal("access extraction changed scope")
		}
		sub[0].Stream = "mutated"
		if want.WorldSubscribe[0].Stream != "camera-a" {
			t.Fatal("subscription slice aliased caller")
		}
	}
}

func TestWorldLegacyPublicationAnnotationsRemainReadable(t *testing.T) {
	pub := []WorldPublication{{ComponentType: "wendy.sensor.scalar.v1", Stream: "gas"}}
	raw, _ := json.Marshal(pub)
	legacy := "publish=" + base64.RawURLEncoding.EncodeToString(raw)
	got := ParseEntitlementAnnotation(EntitlementWorld, legacy)
	if !reflect.DeepEqual(got.WorldPublish, pub) || len(got.WorldSubscribe) != 0 {
		t.Fatal("legacy publication changed")
	}
	if ValidateWorldAccess(got.WorldPublish, got.WorldSubscribe) != nil {
		t.Fatal("legacy scope invalid")
	}
}

func TestWorldQualifiedSubscriptionsPreserveExactDevice(t *testing.T) {
	device := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222"
	local := WorldSubscription{App: "com.example.gas", Service: "sensor", ComponentType: "wendy.sensor.scalar.v1", Stream: "gas"}
	remote := local
	remote.Device = device
	want := Entitlement{Type: EntitlementWorld, WorldSubscribe: []WorldSubscription{local, remote}}
	encoded := EntitlementAnnotationValue(want)
	if got := ParseEntitlementAnnotation(EntitlementWorld, encoded); !reflect.DeepEqual(got, want) {
		t.Fatalf("device selectors changed: %+v", got)
	}
	oldJSON := `{"version":2,"subscribe":[{"app":"com.example.gas","service":"sensor","componentType":"wendy.sensor.scalar.v1","stream":"gas"}]}`
	if got := encodeWorldAccess(nil, []WorldSubscription{local}); got != "access="+base64.RawURLEncoding.EncodeToString([]byte(oldJSON)) {
		t.Fatal("omitted device changed existing annotation encoding")
	}
	for _, bad := range []string{"local", "*", device + "/", device + "?x=1", strings.Replace(device, "/device/", "/user/", 1), strings.Replace(device, "wendy.sh", "other.sh", 1), strings.Replace(device, "22222222-2222-4222-8222-222222222222", "short", 1)} {
		invalid := remote
		invalid.Device = bad
		if ValidateWorldAccess(nil, []WorldSubscription{invalid}) == nil {
			t.Fatalf("accepted malformed principal %q", bad)
		}
	}
	if ValidateWorldAccess(nil, []WorldSubscription{remote, remote}) == nil {
		t.Fatal("duplicate qualified selector accepted")
	}
	large := make([]WorldSubscription, 32)
	for i := range large {
		large[i] = remote
		large[i].Stream = fmt.Sprint(i)
	}
	if ValidateWorldAccess(nil, large) == nil {
		t.Fatal("qualified selectors bypassed containerd label limit")
	}
}

func TestWorldAccessAnnotationRequiresCanonicalVersionTwo(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"subscribe":[{"app":"a","service":"b","componentType":"c","stream":"s"}]}`,
		`{"version":2,"version":2,"publish":[{"componentType":"p","stream":"s"}]}`,
		`{"version":2,"publish":[],"subscribe":[]}`,
		`{"version":2,"subscribe":[{"app":"a","service":"b","componentType":"c","stream":"s","tenant":"other"}]}`,
		`{"version":2, "publish":[{"componentType":"p","stream":"s"}]}`,
		`{"version":2,"publish":null,"subscribe":[{"app":"a","service":"b","componentType":"c","stream":"s"}]}`,
	} {
		got := ParseEntitlementAnnotation(EntitlementWorld, "access="+base64.RawURLEncoding.EncodeToString([]byte(raw)))
		if ValidateWorldAccess(got.WorldPublish, got.WorldSubscribe) == nil {
			t.Fatalf("accepted noncanonical access: %s", raw)
		}
	}
}

func TestWorldSubscriptionsAreExactAndBounded(t *testing.T) {
	valid := WorldSubscription{App: "com.example.sensors", Service: "gas", ComponentType: "wendy.sensor.scalar.v1", Stream: "gas"}
	for name, edit := range map[string]func(*WorldSubscription){"app": func(s *WorldSubscription) { s.App = "*" }, "service": func(s *WorldSubscription) { s.Service = "" }, "wildcard": func(s *WorldSubscription) { s.Stream = "*" }, "control": func(s *WorldSubscription) { s.Stream = "gas\n" }} {
		t.Run(name, func(t *testing.T) {
			bad := valid
			edit(&bad)
			if ValidateWorldAccess(nil, []WorldSubscription{bad}) == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
	if ValidateWorldAccess(nil, nil) == nil || ValidateWorldAccess(nil, []WorldSubscription{valid, valid}) == nil || ValidateWorldAccess(nil, make([]WorldSubscription, 33)) == nil {
		t.Fatal("empty, duplicate or over-count scope accepted")
	}
	short := make([]WorldSubscription, 32)
	for i := range short {
		short[i] = WorldSubscription{App: "a", Service: "b", ComponentType: "c", Stream: fmt.Sprint(i)}
	}
	if err := ValidateWorldAccess(nil, short); err != nil {
		t.Fatalf("32 bounded selectors refused: %v", err)
	}
	for i := range short {
		short[i].App = strings.Repeat("a", 253)
		short[i].ComponentType = strings.Repeat("c", 128)
	}
	if ValidateWorldAccess(nil, short) == nil {
		t.Fatal("containerd label overflow accepted")
	}
}

func TestWorldPublicationScopesAreExactAndBounded(t *testing.T) {
	valid := WorldPublication{ComponentType: "wendy.pose.v1", Stream: "people"}
	for _, scopes := range [][]WorldPublication{nil, {valid, valid}, {{ComponentType: "*", Stream: "people"}}, {{ComponentType: "pose", Stream: "people\n"}}, {{ComponentType: "pose", Stream: strings.Repeat("a", 129)}}, make([]WorldPublication, 9)} {
		if ValidateWorldPublications(scopes) == nil {
			t.Fatalf("accepted %+v", scopes)
		}
	}
	if _, _, err := WorldPublications([]Entitlement{{Type: EntitlementWorld, WorldPublish: []WorldPublication{valid}}, {Type: EntitlementWorld, WorldPublish: []WorldPublication{valid}}}); err == nil {
		t.Fatal("duplicate World entitlement accepted")
	}
}

func TestWorldCgroupScopeIncludesExactDeploymentIdentity(t *testing.T) {
	first, err := WorldCgroupSuffix("com.example.camera", "tracker", 1)
	if err != nil || first != "com.example.camera@tracker.world-0000000000000001" {
		t.Fatalf("%s: %v", first, err)
	}
	second, _ := WorldCgroupSuffix("com.example.camera", "tracker", 2)
	if first == second {
		t.Fatal("replacement reused cgroup identity")
	}
	if _, err = WorldCgroupSuffix("com.example.camera", "", 1); err == nil {
		t.Fatal("implicit service accepted")
	}
	if _, err = WorldCgroupSuffix("com.example.camera", "tracker", 0); err == nil {
		t.Fatal("missing generation accepted")
	}
}
