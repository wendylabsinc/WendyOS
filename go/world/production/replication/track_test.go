package replication

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testtrack"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func TestTypedTrackTransportDoesNotAdmitGenericResults(t *testing.T) {
	now := time.Unix(1794400000, 0).UTC()
	scope := serviceorigin.Scope{Tenant: testidentity.Tenant, Site: "office", Device: "spiffe://wendy.sh/tenant/" + testidentity.Tenant + "/device/22222222-2222-4222-8222-222222222222", App: "com.example.people", Service: "people", DeploymentGeneration: 1, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: resultrecord.ComponentType, Stream: "tracks"}}}
	request := testtrack.Request(t, scope, world.FrameRef{ID: "floor", Revision: "1"}, "repair", now, now)
	record := serviceorigin.Record{Scope: scope, Publication: request}
	until, err := RecordInterpretationUntil(record)
	if err != nil || !until.Equal(now.Add(4*time.Second)) {
		t.Fatal(until, err)
	}
	t.Run("other-origin-input", func(t *testing.T) {
		changed := record
		changed.Scope.Device = "spiffe://wendy.sh/tenant/" + testidentity.Tenant + "/device/33333333-3333-4333-8333-333333333333"
		if _, err := RecordInterpretationUntil(changed); err == nil {
			t.Fatal("third-origin input accepted")
		}
	})
	for _, mode := range []string{"unknown-schema", "unknown-payload-field", "changed-input", "extended-ttl", "tombstone-payload"} {
		t.Run(mode, func(t *testing.T) {
			changed := record
			r, err := resultrecord.Decode(request.Payload)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "unknown-schema":
				r.ResultSchema = "wendy.unknown.v1"
			case "unknown-payload-field":
				var object map[string]any
				_ = json.Unmarshal(r.ResultJSON, &object)
				object["appearance"] = "forbidden"
				r.ResultJSON, _ = json.Marshal(object)
			case "changed-input":
				r.Inputs[0].Version.Sequence++
			case "extended-ttl":
				changed.Publication.TTL = 2 * time.Second
			case "tombstone-payload":
				changed.Publication.Tombstone = true
			}
			changed.Publication.Payload, err = resultrecord.Encode(r)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = RecordInterpretationUntil(changed); err == nil {
				t.Fatal("malformed typed track accepted")
			}
		})
	}
	record.Publication.Tombstone, record.Publication.Payload = true, nil
	if until, err = RecordInterpretationUntil(record); err != nil || !until.IsZero() {
		t.Fatal(until, err)
	}
}
