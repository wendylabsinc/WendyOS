package appv1

import (
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/protobuf/proto"
)

func TestDeviceQualifiedReadsPreserveOriginWithoutSelectingCallerScope(t *testing.T) {
	c := testClockReply()
	remote := strings.Replace(c.Device, "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", 1)
	selector := &SourceSelector{App: "com.example.sensor", Service: "camera", ComponentType: "wendy.sensor.scalar.v1", Stream: "gas", Device: remote}
	wire, err := proto.Marshal(selector)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SourceSelector
	if proto.Unmarshal(wire, &decoded) != nil || decoded.Device != remote {
		t.Fatal("device lost in generated wire contract")
	}
	query, err := DecodeQuery(&QueryRequest{Sources: []*SourceSelector{&decoded}, Limit: 1})
	if err != nil || query.Sources[0].Device != remote {
		t.Fatal("query replaced explicit device", err)
	}
	source, _, err := DecodeGetEvidence(&GetEvidenceRequest{Source: selector, Sha256: strings.Repeat("a", 64), Length: 1})
	if err != nil || source.Device != remote {
		t.Fatal("media selector replaced explicit device", err)
	}
	scope := serviceorigin.Scope{Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: c.App, Service: c.Service, ExecutionDomain: c.ExecutionDomain, DeploymentGeneration: c.DeploymentGeneration}
	request := &GetRecordProofRequest{Source: selector, Entity: "sample", Epoch: 1, Sequence: 1, EnvelopeSha256: strings.Repeat("a", 64), RecordSha256: strings.Repeat("b", 64)}
	ref, err := DecodeGetRecordProof(request, scope)
	if err != nil || ref.Key.Source.Device != remote || ref.Key.Tenant != scope.Tenant || ref.Key.Site != scope.Site {
		t.Fatal("proof decoder erased origin or changed commissioned scope", err)
	}
	request.Source = proto.Clone(selector).(*SourceSelector)
	request.Source.Device = strings.Replace(remote, c.Tenant, "44444444-4444-4444-8444-444444444444", 1)
	if _, err := DecodeGetRecordProof(request, scope); err == nil {
		t.Fatal("qualified proof escaped socket tenant")
	}
}
