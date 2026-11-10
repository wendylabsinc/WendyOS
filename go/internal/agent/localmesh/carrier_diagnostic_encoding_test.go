package localmesh

import (
	"strings"
	"testing"
)

func TestCarrierDiagnosticOversizeObservationIsDroppedIntact(t *testing.T) {
	if _, ok := encodeCarrierDiagnostic(warmCarrierPrefix, strings.Repeat("x", 8192)); ok {
		t.Fatal("oversized diagnostic could reach the truncating logger")
	}
}
