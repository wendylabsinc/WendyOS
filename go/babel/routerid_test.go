package babel

import (
	"net"
	"testing"
)

func TestRouterIDFromMAC(t *testing.T) {
	for _, tc := range []struct {
		name string
		mac  net.HardwareAddr
		want RouterID
		err  bool
	}{
		{"factory", net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}, 0x0000aabbccddeeff, false},
		{"low", net.HardwareAddr{0, 0, 0, 0, 0, 1}, 0x00000000000001, false},
		{"all-zero", net.HardwareAddr{0, 0, 0, 0, 0, 0}, 0, true},
		{"all-ones-mac", net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, 0x0000ffffffffffff, false},
		{"empty", nil, 0, true},
		{"short", net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee}, 0, true},
		{"long", net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00}, 0, true},
	} {
		id, err := RouterIDFromMAC(tc.mac)
		if tc.err {
			if err == nil {
				t.Errorf("%s: RouterIDFromMAC accepted an unusable MAC", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: RouterIDFromMAC: %v", tc.name, err)
			continue
		}
		if id != tc.want {
			t.Errorf("%s: RouterIDFromMAC = %016x, want %016x", tc.name, id, tc.want)
		}
		if id == 0 || id == RouterID(^uint64(0)) {
			t.Errorf("%s: RouterIDFromMAC produced an engine-prohibited ID", tc.name)
		}
	}
}

func TestRouterIDFromRandom(t *testing.T) {
	for _, seed := range [][6]byte{
		{},
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		{0x01, 0x23, 0x45, 0x67, 0x89, 0xab},
	} {
		id := RouterIDFromRandom(seed)
		if id>>48 != 1 {
			t.Errorf("RouterIDFromRandom(%x) tag = %04x, want 0001", seed, id>>48)
		}
		if id == 0 || id == RouterID(^uint64(0)) {
			t.Errorf("RouterIDFromRandom(%x) produced an engine-prohibited ID", seed)
		}
		if got := RouterIDFromRandom(seed); got != id {
			t.Errorf("composition not deterministic for %x", seed)
		}
		// Both derivation paths must be accepted by the engine, and the
		// resulting instance must be usable for routing.
		e, err := New(Config{RouterID: id, MaxLinks: 2, MaxRoutes: 8, MaxSources: 8, MaxRequests: 4})
		if err != nil {
			t.Fatalf("New with random-derived ID %016x: %v", id, err)
		}
		if e == nil {
			t.Fatal("nil engine")
		}
	}
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	id, err := RouterIDFromMAC(mac)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{RouterID: id, MaxLinks: 2, MaxRoutes: 8, MaxSources: 8, MaxRequests: 4}); err != nil {
		t.Fatalf("New with MAC-derived ID %016x: %v", id, err)
	}
}

// TestCheckpointRouterIDPinning: a checkpoint belongs to exactly one router ID.
// Loading another instance's state must fail rather than mix histories.
func TestCheckpointRouterIDPinning(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	id, err := RouterIDFromMAC(mac)
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(Config{RouterID: id, MaxLinks: 2, MaxRoutes: 8, MaxSources: 8, MaxRequests: 4})
	if err != nil {
		t.Fatal(err)
	}
	state := e.Checkpoint()
	if _, err := Restore(Config{RouterID: RouterIDFromRandom([6]byte{1, 2, 3, 4, 5, 6})}, state); err != ErrConfig {
		t.Errorf("Restore with a different router ID = %v, want ErrConfig", err)
	}
	if _, err := Restore(Config{RouterID: id, MaxLinks: 2, MaxRoutes: 8, MaxSources: 8, MaxRequests: 4}, state); err != nil {
		t.Errorf("Restore with the same router ID: %v", err)
	}
}
