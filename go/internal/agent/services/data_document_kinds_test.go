package services

import (
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

// A kind registered in data but missing here would be stored, listed and
// removed, yet never run or reported. This test is what catches that.
func TestEveryStoredKindHasAHandler(t *testing.T) {
	for _, kind := range data.StoredKinds() {
		handler, ok := documentKindHandlers[kind]
		if !ok {
			t.Errorf("stored kind %q has no entry in documentKindHandlers", kind)
			continue
		}
		if handler.ready == nil || handler.applied == nil || handler.status == nil {
			t.Errorf("documentKindHandlers[%q] is incomplete", kind)
		}
	}
	for kind := range documentKindHandlers {
		if !slices.Contains(data.StoredKinds(), kind) {
			t.Errorf("documentKindHandlers has %q, which the data registry does not store", kind)
		}
	}
}
