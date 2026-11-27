package node

import (
	"encoding/json"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"net/http/httptest"
	"strings"
	"testing"
)

// These exercise the actual HTTP response encoder at its hard limit. The large
// payload is a serialization fixture, not an origin-authorized publication.
func TestPartialResponseBudgetIncludesScopeAndNewline(t *testing.T) {
	policy := nodePolicy("wendy.gas.v0")
	for i := 0; i < 32; i++ {
		policy.Selection.Entities = append(policy.Selection.Entities, fmt.Sprintf("%03d", i)+strings.Repeat("x", 253))
	}
	p, err := policy.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"query", "replica"} {
		t.Run(kind, func(t *testing.T) {
			value := func(n int) any {
				component := world.Component{Payload: json.RawMessage(`"` + strings.Repeat("x", n) + `"`)}
				if kind == "query" {
					return QueryResponse{ReplicaScope: p.Scope(), Components: []world.Component{component}, Cursor: strings.Repeat("a", 64)}
				}
				return ReplicationResponse{ReplicaScope: p.Scope(), Records: []authority.SignedRecord{{Component: component}}}
			}
			// An array that satisfied the former 256-byte wrapper reserve still exceeds
			// the limit once the several-KiB partial scope is included.
			empty := value(0)
			base, _ := json.Marshal(empty)
			var arrayBase []byte
			if kind == "query" {
				arrayBase, _ = json.Marshal(empty.(QueryResponse).Components)
			} else {
				arrayBase, _ = json.Marshal(empty.(ReplicationResponse).Records)
			}
			formerFit := MaxResponseBytes - 256 - len(arrayBase)
			if _, err := marshalResponse(value(formerFit)); err == nil {
				t.Fatal("scope omitted from budget")
			}
			fit := MaxResponseBytes - 1 - len(base)
			response := httptest.NewRecorder()
			writeJSON(response, 200, value(fit))
			if response.Code != 200 || response.Body.Len() != MaxResponseBytes {
				t.Fatalf("exact boundary: %d %d", response.Code, response.Body.Len())
			}
			response = httptest.NewRecorder()
			writeJSON(response, 200, value(fit+1))
			if response.Code == 200 || response.Body.Len() > MaxResponseBytes {
				t.Fatalf("newline exceeded limit: %d %d", response.Code, response.Body.Len())
			}
		})
	}
}
