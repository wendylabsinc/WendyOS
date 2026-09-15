package ipcam

import "testing"

// The MCU camera band must sit above the IP band and inside EnsureNode's guard.
func TestMCUBandBounds(t *testing.T) {
	if MCUBandStart <= IDBandEnd {
		t.Fatalf("MCU band %d overlaps IP band ending at %d", MCUBandStart, IDBandEnd)
	}
	if MCUBandEnd < MCUBandStart {
		t.Fatalf("MCU band end %d before start %d", MCUBandEnd, MCUBandStart)
	}
	// The sweep only reaps below LoopbackBandStart, so the MCU band is safe.
	if MCUBandStart < LoopbackBandStart {
		t.Fatalf("MCU band would be swept")
	}
}
