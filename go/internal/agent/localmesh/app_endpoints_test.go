package localmesh

import (
	"encoding/json"
	"testing"
)

func TestAppEndpointsAreSignedValidatedAndOwned(t *testing.T) {
	directory, chain, key, device, now := directoryFixture(t)
	manifest := routeManifest(device, now)
	manifest.AppEndpoints = []AppEndpoint{{Transport: AppTLS, Port: AppSessionTLSPort}}
	wire, err := SignManifest(manifest, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = directory.Accept(wire, now); err != nil {
		t.Fatal(err)
	}
	snapshot := directory.Snapshot(now)
	if len(snapshot) != 1 || len(snapshot[0].AppEndpoints) != 1 {
		t.Fatal(snapshot)
	}
	snapshot[0].AppEndpoints[0].Port = 1
	if directory.Snapshot(now)[0].AppEndpoints[0].Port != AppSessionTLSPort {
		t.Fatal("capability aliases directory")
	}
	manifest.AppEndpoints[0].Transport = AppQUIC
	modified, _ := json.Marshal(manifest)
	wire.Body = modified
	if _, err = directory.Accept(wire, now); err == nil {
		t.Fatal("unsigned transport mutation accepted")
	}
	for _, bad := range [][]AppEndpoint{
		{{Transport: AppTLS, Port: 0}}, {{Transport: "unknown", Port: 1}},
		{{Transport: AppTLS, Port: 1}, {Transport: AppTLS, Port: 2}},
		{{Transport: AppTLS, Port: 1}, {Transport: AppQUIC, Port: 2}, {Transport: AppTLS, Port: 3}},
	} {
		manifest.AppEndpoints = bad
		if _, err := SignManifest(manifest, chain, key, now); err == nil {
			t.Fatalf("accepted invalid capabilities: %+v", bad)
		}
	}
}

func TestExplicitEmptyAppCapabilitiesSurviveSignedRoster(t *testing.T) {
	directory, chain, key, device, now := directoryFixture(t)
	manifest := routeManifest(device, now)
	manifest.AppEndpoints = []AppEndpoint{}
	wire, err := SignManifest(manifest, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = directory.Accept(wire, now); err != nil {
		t.Fatal(err)
	}
	snapshot := directory.Snapshot(now)
	if len(snapshot) != 1 || snapshot[0].AppEndpoints == nil || len(snapshot[0].AppEndpoints) != 0 {
		t.Fatal("explicit unavailable capability lost", snapshot)
	}
}

func TestAgentMTLSCapabilityIsSignedAndValidated(t *testing.T) {
	directory, chain, key, device, now := directoryFixture(t)
	manifest := routeManifest(device, now)
	manifest.AgentTransport = AgentMTLS
	wire, err := SignManifest(manifest, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = directory.Accept(wire, now); err != nil {
		t.Fatal(err)
	}
	if directory.Snapshot(now)[0].AgentTransport != AgentMTLS {
		t.Fatal("lost agent capability")
	}
	manifest.AgentTransport = ""
	wire.Body, _ = json.Marshal(manifest)
	if _, err = directory.Accept(wire, now); err == nil {
		t.Fatal("unsigned capability removal accepted")
	}
	manifest.AgentTransport = "plaintext"
	if _, err = SignManifest(manifest, chain, key, now); err == nil {
		t.Fatal("unsupported agent transport accepted")
	}
}
