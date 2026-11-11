package vpn

import (
	"github.com/miekg/dns"
	"strings"
	"testing"
)

func dnsNameReply(t *testing.T, name string, routes []Route) *dns.Msg {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), dns.TypeA)
	wire, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	wire, err = AnswerDNS(wire, routes)
	if err != nil {
		t.Fatal(err)
	}
	response := new(dns.Msg)
	if err = response.Unpack(wire); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestShortDNSAliasesAndExactFriendlyLabels(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const first = "22222222-2222-4222-8222-222222222222"
	const second = "33333333-3333-4333-8333-333333333333"
	routes, err := BuildRoutes([]Route{{Selector: "mesh://" + tenant + "/" + first, Name: "jetson-a"}, {Selector: "cloud://api.wendy.dev/tenant/" + tenant + "/asset/" + second, Name: "jetson--a", Principal: "spiffe://wendy.sh/tenant/" + tenant + "/device/" + second}})
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"wendy.internal", "mesh.wendy.internal", "mesh.wendy.dev", "cloud.wendy.dev"} {
		response := dnsNameReply(t, strings.ToUpper("device-"+first+"."+suffix), routes)
		if response.Rcode != dns.RcodeSuccess || len(response.Answer) != 1 || response.Answer[0].(*dns.A).A.String() != routes[0].Address {
			t.Fatalf("%s: %s", suffix, response)
		}
	}
	for i, name := range []string{"jetson-a.wendy.internal", "JETSON--A.WENDY.INTERNAL"} {
		response := dnsNameReply(t, name, routes)
		if len(response.Answer) != 1 || response.Answer[0].(*dns.A).A.String() != routes[i].Address {
			t.Fatalf("%s: %s", name, response)
		}
	}
}

func TestShortFriendlyDNSRefusesAmbiguousAndCrossTenantNames(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const other = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const first = "22222222-2222-4222-8222-222222222222"
	const second = "33333333-3333-4333-8333-333333333333"
	for _, tt := range []struct{ name, tenant string }{{"JETSON", tenant}, {"another-device", other}} {
		routes, err := BuildRoutes([]Route{{Selector: "mesh://" + tenant + "/" + first, Name: "jetson"}, {Selector: "mesh://" + tt.tenant + "/" + second, Name: tt.name}})
		if err != nil {
			t.Fatal(err)
		}
		if response := dnsNameReply(t, "jetson.wendy.internal", routes); response.Rcode != dns.RcodeNameError || len(response.Answer) != 0 {
			t.Fatalf("%+v: %s", tt, response)
		}
		if response := dnsNameReply(t, "device-"+first+".wendy.internal", routes); len(response.Answer) != 1 {
			t.Fatalf("UUID should remain unambiguous: %s", response)
		}
	}
}

func TestShortDNSDoesNotTreatLegacyAssetNumberAsUUID(t *testing.T) {
	routes, err := BuildRoutes([]Route{{Selector: "cloud://api.wendy.dev/org/7/asset/42", Name: "remote"}})
	if err != nil {
		t.Fatal(err)
	}
	if response := dnsNameReply(t, "device-42.cloud.wendy.dev", routes); len(response.Answer) != 1 {
		t.Fatal(response)
	}
	if response := dnsNameReply(t, "device-42.wendy.internal", routes); response.Rcode != dns.RcodeNameError {
		t.Fatal(response)
	}
}

func TestShortFriendlyNamesCanStartWithDevice(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const id = "22222222-2222-4222-8222-222222222222"
	for _, name := range []string{"device-camera", "device-42"} {
		routes, err := BuildRoutes([]Route{{Selector: "mesh://" + tenant + "/" + id, Name: name}})
		if err != nil {
			t.Fatal(err)
		}
		if response := dnsNameReply(t, name+".wendy.internal", routes); len(response.Answer) != 1 {
			t.Fatal(response)
		}
	}
}

func TestCloudAssetUUIDDoesNotBecomeDeviceIdentityAlias(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const id = "22222222-2222-4222-8222-222222222222"
	cloud := Route{Selector: "cloud://api.wendy.dev/tenant/" + tenant + "/asset/" + id, Name: "jetson"}
	routes, err := BuildRoutes([]Route{cloud})
	if err != nil {
		t.Fatal(err)
	}
	if routes[0].Host != "device-"+id+".cloud.wendy.dev" {
		t.Fatal(routes)
	}
	if response := dnsNameReply(t, "device-"+id+".wendy.internal", routes); response.Rcode != dns.RcodeNameError {
		t.Fatal(response)
	}
	if response := dnsNameReply(t, "jetson.wendy.internal", routes); response.Rcode != dns.RcodeNameError {
		t.Fatal("unbound Cloud route gained a private friendly alias", response)
	}

	// Asset and mesh identities may differ in older Cloud inventories. Keeping
	// their explicit namespaces distinct also preserves mixed route configs.
	routes, err = BuildRoutes([]Route{cloud, {Selector: "mesh://" + tenant + "/" + id, Name: "jetson"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		suffix string
		index  int
	}{{"wendy.internal", 1}, {"cloud.wendy.dev", 0}} {
		response := dnsNameReply(t, "device-"+id+"."+tt.suffix, routes)
		if len(response.Answer) != 1 || response.Answer[0].(*dns.A).A.String() != routes[tt.index].Address {
			t.Fatalf("%s: %s", tt.suffix, response)
		}
	}
}

func TestFriendlyDNSRejectsUnicodeCaseAliases(t *testing.T) {
	const target = "mesh://11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222"
	for _, tt := range []struct{ device, query string }{{"kelvin", "Kelvin"}, {"Kelvin", "kelvin"}} {
		routes, err := BuildRoutes([]Route{{Selector: target, Name: tt.device}})
		if err != nil {
			t.Fatal(err)
		}
		response := dnsNameReply(t, tt.query+".wendy.internal", routes)
		if response.Rcode != dns.RcodeNameError || len(response.Answer) != 0 {
			t.Fatal(response)
		}
	}
}

func TestPersistedMeshDNSAliasesRemainValid(t *testing.T) {
	routes, err := BuildRoutes([]Route{{Selector: "mesh://11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222", Name: "jetson"}})
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Endpoint: "127.0.0.1:8080", Token: strings.Repeat("a", 64), Routes: routes}
	for _, suffix := range []string{"mesh.wendy.dev", "mesh.wendy.internal", "cloud.wendy.dev"} {
		config.Routes[0].Host = "device-22222222-2222-4222-8222-222222222222." + suffix
		if err := config.Validate(); err != nil {
			t.Fatalf("%s: %v", suffix, err)
		}
	}
	config.Routes[0].Host = "device-33333333-3333-4333-8333-333333333333.mesh.wendy.dev"
	if config.Validate() == nil {
		t.Fatal("accepted different persisted device identity")
	}
}

func TestVerifiedCloudIdentityAliasesAndMeshPreference(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const asset = "22222222-2222-4222-8222-222222222222"
	const device = "33333333-3333-4333-8333-333333333333"
	cloud := Route{Selector: "cloud://api.wendy.dev/tenant/" + tenant + "/asset/" + asset, Name: "jetson", Principal: "spiffe://wendy.sh/tenant/" + tenant + "/device/" + device}
	mesh := Route{Selector: "mesh://" + tenant + "/" + device, Name: "JETSON"}
	for _, input := range [][]Route{{cloud}, {cloud, mesh}, {mesh, cloud}} {
		routes, err := BuildRoutes(input)
		if err != nil {
			t.Fatal(err)
		}
		want := routes[0]
		for _, r := range routes {
			if strings.HasPrefix(r.Selector, "mesh:") {
				want = r
			}
		}
		for _, name := range []string{"device-" + device + ".wendy.internal", "jetson.wendy.internal"} {
			response := dnsNameReply(t, name, routes)
			if len(response.Answer) != 1 || response.Answer[0].(*dns.A).A.String() != want.Address {
				t.Fatal(response)
			}
		}
		if response := dnsNameReply(t, "device-"+asset+".wendy.internal", routes); response.Rcode != dns.RcodeNameError {
			t.Fatal("asset row became private identity", response)
		}
		if response := dnsNameReply(t, "device-"+asset+".cloud.wendy.dev", routes); len(response.Answer) != 1 {
			t.Fatal("lost legacy asset alias", response)
		}
	}
	cloud.Principal = "spiffe://wendy.sh/tenant/" + tenant + "/device/" + asset
	routes, err := BuildRoutes([]Route{cloud, mesh})
	if err != nil {
		t.Fatal(err)
	}
	if response := dnsNameReply(t, "jetson.wendy.internal", routes); response.Rcode != dns.RcodeNameError {
		t.Fatal("different principals merged", response)
	}
	cloud.Principal = "spiffe://wendy.sh/tenant/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/device/" + device
	if _, err := BuildRoutes([]Route{cloud}); err == nil {
		t.Fatal("accepted foreign principal")
	}
	mesh.Principal = "spiffe://wendy.sh/tenant/" + tenant + "/device/" + asset
	if _, err := BuildRoutes([]Route{mesh}); err == nil {
		t.Fatal("accepted mesh selector/principal mismatch")
	}
}
func TestPrivateZoneApexIsAuthoritative(t *testing.T) {
	response := dnsNameReply(t, "wendy.internal", nil)
	if response.Rcode != dns.RcodeNameError || !response.Authoritative {
		t.Fatal(response)
	}
}
