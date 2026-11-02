package localmesh

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"strings"
	"testing"
)

func TestPeerRawOperatorOptionalEntriesCannotDisappearInProjection(t *testing.T) {
	for _, kind := range []string{"endpoint", "service"} {
		t.Run(kind, func(t *testing.T) {
			_, wire, _, _, _ := projectionPeerFixture(t, func(f map[string]netwire.RawMessage) {
				f["o"] = projectionPeerEncode(t, &OperatorOwner{})
				f["a"] = projectionPeerEncode(t, []any{})
				f["s"] = projectionPeerEncode(t, []any{})
				if kind == "endpoint" {
					f["a"] = projectionPeerEncode(t, []any{map[string]any{"t": "future-v2"}})
				} else {
					f["s"] = projectionPeerEncode(t, []any{map[string]any{"m": "lab", "a": "wendy.mesh.system", "s": "future-capability"}})
				}
			})
			if _, err := DecodeManifest(wire.Body); err == nil || !strings.Contains(err.Error(), "operator origin does not advertise device capabilities") {
				t.Fatalf("operator raw optional gate: %v", err)
			}
		})
	}
}
