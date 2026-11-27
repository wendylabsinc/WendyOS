package agentmodule

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
)

func TestHazardPhysicalCapCannotExtendOriginalInput(t *testing.T) {
	deadline := moduleTestNow.Add(time.Minute)
	for _, tc := range []struct {
		name, body string
		denied     bool
		want       time.Time
	}{
		{"legacy", `{}`, false, deadline},
		{"null", `{"physical_valid_until":null}`, true, deadline},
		{"malformed", `{"physical_valid_until":"bad"}`, true, deadline},
		{"extended", `{"physical_valid_until":"` + deadline.Add(time.Nanosecond).Format(time.RFC3339Nano) + `"}`, true, deadline},
		{"equal", `{"physical_valid_until":"` + deadline.Format(time.RFC3339Nano) + `"}`, false, deadline},
		{"shorter", `{"physical_valid_until":"` + moduleTestNow.Format(time.RFC3339Nano) + `"}`, false, moduleTestNow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := resultrecord.Validation{Result: resultrecord.Result{ResultSchema: fusion.HazardType, ResultJSON: json.RawMessage(tc.body)}, PhysicalValidUntil: deadline}
			err := applyHazardPhysicalCap(&v)
			if (err != nil) != tc.denied || !v.PhysicalValidUntil.Equal(tc.want) {
				t.Fatal("wrong physical bound", v.PhysicalValidUntil, err)
			}
		})
	}
}

// This tests local generic-result admission, not the separately tested typed
// hazard validator used by remote replication.
func TestLocalHazardReadUsesEarlierPhysicalCap(t *testing.T) {
	f := setupModule(t, archiveConfig)
	writer := moduleDial(t, f.mount(t, 0, 1).SocketPath)
	source := modulePublish(t, writer, moduleRequest("gas"))
	ref := archiveReference(t, source)
	request := derivedRequest(t, []proofref.Reference{ref}, 10*time.Second, moduleTestNow.Add(time.Minute).UnixMilli())
	result, err := resultrecord.Decode(request.PayloadJson)
	if err != nil {
		t.Fatal(err)
	}
	result.ResultSchema = fusion.HazardType
	result.ResultJSON, err = json.Marshal(map[string]any{"physical_valid_until": moduleTestNow.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	request.PayloadJson, err = resultrecord.Encode(result)
	if err != nil {
		t.Fatal(err)
	}
	claim := modulePublish(t, archiveWorker(t, f, true), request)
	claimSource := sourceSelector(archiveReference(t, claim))
	reader := moduleDial(t, accessMount(t, f, sourceSelector(ref), claimSource).SocketPath)
	if got := accessQuery(t, reader, []serviceaccess.Source{claimSource}, 8, ""); len(got.SignedRecordJson) != 1 {
		t.Fatal("fresh result missing")
	}
	f.clock.mu.Lock()
	f.clock.window.Earliest = f.clock.window.Earliest.Add(2 * time.Second)
	f.clock.window.Latest = f.clock.window.Latest.Add(2 * time.Second)
	f.clock.window.Monotonic += 2 * time.Second
	f.clock.mu.Unlock()
	if got := accessQuery(t, reader, []serviceaccess.Source{claimSource}, 8, ""); len(got.SignedRecordJson) != 0 {
		t.Fatal("local result outlived its signed physical cap")
	}
	if got := accessQuery(t, reader, []serviceaccess.Source{sourceSelector(ref)}, 8, ""); len(got.SignedRecordJson) != 1 {
		t.Fatal("test expired original instead of the shorter result")
	}
}
