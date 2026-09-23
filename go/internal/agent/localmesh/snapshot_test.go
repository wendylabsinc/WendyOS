package localmesh

import "testing"

func TestSnapshotHasCheaperDirectLink(t *testing.T) {
	s := NodeSnapshot{Links: []PeerLink{
		{Asset: 445, Cost: 4096},
		{Asset: 445, Cost: 256},
		{Asset: 358, Cost: 4096},
	}}
	if !s.HasCheaperLink(445, 4096) {
		t.Fatal("same asset's TCP link should make BLE redundant")
	}
	if s.HasCheaperLink(358, 4096) {
		t.Fatal("an unrelated peer must not suppress BLE")
	}
	if s.HasCheaperLink(445, 256) {
		t.Fatal("an equally priced link is not cheaper")
	}
}
