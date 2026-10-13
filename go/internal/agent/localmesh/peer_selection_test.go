package localmesh

import (
	"testing"
	"time"
)

func TestPeerSelectionLANAndRadioDiversity(t *testing.T) {
	now := time.Now()
	snapshot := NodeSnapshot{}
	p := NewPeerSelection(func() NodeSnapshot { return snapshot })
	p.now = func() time.Time { return now }
	p.Seen(10, RadioNAN)
	if !p.AllowRadio(10, RadioNAN) || !p.AllowRadio(10, RadioBLE) {
		t.Fatal("an available radio peer was rejected")
	}
	snapshot.Links = []PeerLink{{Asset: 10, Cost: 512}}
	if !p.AllowRadio(10, RadioBLE) {
		t.Fatal("a second radio should provide failover when there is no other candidate")
	}
	p.Seen(11, RadioBLE)
	if p.AllowRadio(10, RadioBLE) || !p.AllowRadio(11, RadioBLE) {
		t.Fatal("a different peer should fill the open slot before duplicating asset 10")
	}
	p.Failed(11, RadioBLE)
	p.Seen(11, RadioBLE)
	if !p.AllowRadio(10, RadioBLE) {
		t.Fatal("continuing advertisements from a failed candidate suppressed fallback")
	}
	now = now.Add(31 * time.Second)
	if p.AllowRadio(10, RadioBLE) {
		t.Fatal("candidate was not reconsidered after failure cooldown")
	}
	p.Connected(11, RadioBLE)
	if p.AllowRadio(10, RadioBLE) {
		t.Fatal("healthy candidate did not reclaim the diverse slot")
	}
	now = now.Add(61 * time.Second)
	if !p.AllowRadio(10, RadioBLE) {
		t.Fatal("expired candidate hints should not suppress fallback")
	}
	snapshot.Links = append(snapshot.Links, PeerLink{Asset: 10, Cost: 128})
	if p.AllowRadio(10, RadioNAN) || p.AllowRadio(10, RadioBLE) || !p.HasLAN(10) {
		t.Fatal("authenticated LAN must displace both radios")
	}
}

func TestPeerSelectionConfiguredTCPAndDirectLAN(t *testing.T) {
	snapshot := NodeSnapshot{Links: []PeerLink{{Asset: 10, Cost: 256}}}
	p := NewPeerSelection(func() NodeSnapshot { return snapshot })
	if p.HasLAN(10) || !p.AllowRadio(10, RadioNAN) || !p.AllowRadio(10, RadioBLE) {
		t.Fatal("configured TCP must coexist with NAN and BLE")
	}
	if p.AllowLAN(10, 256) {
		t.Fatal("configured TCP must not be counted as discovered LAN")
	}
	for _, cost := range []uint16{64, 128} {
		snapshot.Links = append(snapshot.Links, PeerLink{Asset: 10, Cost: cost})
		if !p.HasLAN(10) || p.AllowRadio(10, RadioNAN) || p.AllowRadio(10, RadioBLE) {
			t.Fatalf("direct LAN cost %d must suppress radios", cost)
		}
		snapshot.Links = snapshot.Links[:1]
	}
}

func TestPeerSelectionCapsDistinctPeers(t *testing.T) {
	snapshot := NodeSnapshot{Links: []PeerLink{
		{Asset: 10, Cost: 512}, {Asset: 11, Cost: 4096}, {Asset: 12, Cost: 128},
	}}
	p := NewPeerSelection(func() NodeSnapshot { return snapshot })
	if p.AllowRadio(13, RadioBLE) || p.AllowRadio(11, RadioNAN) {
		t.Fatal("radio selection exceeded the three-distinct-peer target")
	}
	if !p.AllowRadio(10, RadioNAN) {
		t.Fatal("an existing sole radio link must be retained")
	}
	// If simultaneous handshakes temporarily add a fourth asset, the
	// periodic radio watchers shed the excess radio peer. LAN peers win.
	snapshot.Links = append(snapshot.Links, PeerLink{Asset: 13, Cost: 4096})
	if p.AllowRadio(13, RadioBLE) || !p.AllowRadio(10, RadioNAN) {
		t.Fatal("four-peer overshoot did not converge toward three")
	}
	snapshot.Links = append(snapshot.Links, PeerLink{Asset: 13, Cost: 64})
	if p.AllowRadio(11, RadioBLE) {
		t.Fatal("new LAN neighbor did not displace an excess radio peer")
	}
}

func TestPeerSelectionLANReplacesRadioAndSlowerLAN(t *testing.T) {
	snapshot := NodeSnapshot{Links: []PeerLink{
		{Asset: 10, Cost: 4096}, {Asset: 11, Cost: 512}, {Asset: 12, Cost: 4096},
	}}
	p := NewPeerSelection(func() NodeSnapshot { return snapshot })
	if !p.AllowLAN(13, 64) {
		t.Fatal("three radio peers must not block a direct LAN neighbor")
	}
	snapshot.Links = []PeerLink{
		{Asset: 10, Cost: 128}, {Asset: 11, Cost: 128}, {Asset: 12, Cost: 128},
	}
	if !p.AllowLAN(13, 64) {
		t.Fatal("wired neighbor must displace a Wi-Fi link when LAN slots are full")
	}
	snapshot.Links = append(snapshot.Links, PeerLink{Asset: 13, Cost: 64})
	if p.AllowLAN(12, 128) || !p.AllowLAN(13, 64) {
		t.Fatal("LAN overshoot did not converge to three best neighbors")
	}
	if p.AllowLAN(10, 256) {
		t.Fatal("higher-cost LAN duplicate should be rejected")
	}
}
