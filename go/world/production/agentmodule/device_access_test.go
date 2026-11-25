package agentmodule

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
)

func TestQualifiedSourcePolicyRequiresSeparateRemoteAdapter(t *testing.T) {
	policy := basePolicy()
	source := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
	source.Device = policy.Device
	policy.Grants = append(policy.Grants, Grant{App: "com.example.reader", Service: "reader", Subscribe: []serviceaccess.Source{source}})
	if err := policy.validate(); err != nil {
		t.Fatal("explicit local source rejected", err)
	}
	policy.Grants[len(policy.Grants)-1].Subscribe[0].Device = strings.Replace(policy.Device, "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", 1)
	if err := policy.validate(); err == nil {
		t.Fatal("remote source enabled without remote authority/store wiring")
	}
}

func TestAccessQualifiedLocalReadsNeverAliasAnotherDevice(t *testing.T) {
	f := setupModule(t, accessConfig)
	source := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
	writer := moduleDial(t, f.mount(t, 0, 11).SocketPath)
	reader := moduleDial(t, accessMount(t, f, source).SocketPath)
	first := moduleRequest("gas")
	first.Entity = "one"
	want := modulePublish(t, writer, first).SignedRecordJson
	second := moduleRequest("gas")
	second.Entity = "two"
	modulePublish(t, writer, second)
	qualified := source
	qualified.Device = f.config.Policy.Device
	page := accessQuery(t, reader, []serviceaccess.Source{qualified}, 1, "")
	if len(page.SignedRecordJson) != 1 || !bytes.Equal(page.SignedRecordJson[0], want) || page.Next == "" {
		t.Fatal("explicit local request changed original proof or paging")
	}
	if next := accessQuery(t, reader, []serviceaccess.Source{qualified}, 1, page.Next); len(next.SignedRecordJson) != 1 || next.Next != "" {
		t.Fatal("qualified cursor did not finish local query")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	remote := qualified
	remote.Device = strings.Replace(qualified.Device, "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", 1)
	for _, sources := range [][]serviceaccess.Source{{remote}, {source, qualified}} {
		request := &appv1.QueryRequest{Limit: 2}
		for _, s := range sources {
			request.Sources = append(request.Sources, accessSelector(s))
		}
		if out, err := reader.Query(ctx, request); err == nil || out != nil {
			t.Fatal("remote alias or duplicate resolved source accepted")
		}
	}
	if out, err := reader.GetEvidence(ctx, &appv1.GetEvidenceRequest{Source: accessSelector(remote), Sha256: strings.Repeat("a", 64), Length: 1}); err == nil || out != nil {
		t.Fatal("remote media selector fell back to local storage")
	}
	if out, err := reader.GetRecordProof(ctx, &appv1.GetRecordProofRequest{Source: accessSelector(remote), Entity: "one", Epoch: 1, Sequence: 1, EnvelopeSha256: strings.Repeat("a", 64), RecordSha256: strings.Repeat("b", 64)}); err == nil || out != nil {
		t.Fatal("remote proof selector fell back to local archive")
	}
}
