package proofref

import (
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestExactReferenceSyntaxAndSource(t *testing.T) {
	r := Reference{Key: world.Key{Tenant: "11111111-1111-4111-8111-111111111111", Site: "office", Entity: "sensor/gas-1", Type: "wendy.sensor.capture.v1", Source: world.Source{Device: "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222", App: "com.example.sensors", Service: "gas", Stream: "gas-1"}}, Version: world.Version{Epoch: 7, Sequence: 8}, EnvelopeSHA256: strings.Repeat("a", 64), RecordSHA256: strings.Repeat("b", 64)}
	if r.Validate() != nil || r.Source().Component != r.Key.Type || r.ID() != "sha256:"+r.EnvelopeSHA256 {
		t.Fatal("exact reference rejected")
	}
	for _, change := range []func(*Reference){func(r *Reference) { r.Version.Epoch = 0 }, func(r *Reference) { r.Version.Sequence = 0 }, func(r *Reference) { r.Key.Entity = "*" }, func(r *Reference) { r.Key.Source.Stream = "*" }, func(r *Reference) { r.EnvelopeSHA256 = strings.ToUpper(r.EnvelopeSHA256) }, func(r *Reference) { r.RecordSHA256 = "bad" }, func(r *Reference) { r.Key.Source.Device += "/other" }} {
		bad := r
		change(&bad)
		if bad.Validate() == nil {
			t.Fatal("ambiguous reference accepted")
		}
	}
}
