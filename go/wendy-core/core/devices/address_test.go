package devices

import "testing"

func TestLANAgentAddresses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		device    LANDevice
		port      int
		addresses []string
	}{
		{"default", LANDevice{IPAddress: "10.0.0.1", Hostname: "pi.local"}, 50051, []string{"10.0.0.1:50051", "pi.local:50051"}},
		{"TLS advertisement", LANDevice{Port: 50052, IsMTLS: true, IPAddress: "10.0.0.1"}, 50051, []string{"10.0.0.1:50051"}},
		{"USB hostname first", LANDevice{Port: 1234, USB: "en0", IPAddress: "10.0.0.1", Hostname: "pi.local"}, 1234, []string{"pi.local:1234", "10.0.0.1:1234"}},
		{"zoned USB address first", LANDevice{USB: "en0", IPAddress: "fe80::1%en0", Hostname: "pi.local"}, 50051, []string{"[fe80::1%en0]:50051", "pi.local:50051"}},
		{"duplicate names", LANDevice{IPAddress: "pi.local", Hostname: "pi.local"}, 50051, []string{"pi.local:50051"}},
		{"no addresses", LANDevice{}, 50051, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if port := LANAgentPort(tc.device); port != tc.port {
				t.Fatalf("port=%d want %d", port, tc.port)
			}
			got := LANAgentAddresses(tc.device)
			if len(got) != len(tc.addresses) {
				t.Fatalf("addresses=%v want %v", got, tc.addresses)
			}
			for i := range got {
				if got[i] != tc.addresses[i] {
					t.Fatalf("addresses=%v want %v", got, tc.addresses)
				}
			}
			preferred := ""
			if len(got) > 0 {
				preferred = got[0]
			}
			if PreferredLANAddress(tc.device) != preferred {
				t.Fatal("primary address changed")
			}
		})
	}
}

func TestOrderDialCandidates(t *testing.T) {
	t.Run("routable IPv6 outranks link-local and ULA", func(t *testing.T) {
		got := OrderDialCandidates([]string{"fe80::1", "fdc5::1", "2606:4700::1"})
		want := []string{"2606:4700::1", "fe80::1", "fdc5::1"}
		if len(got) != len(want) {
			t.Fatalf("OrderDialCandidates = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("OrderDialCandidates = %v, want %v", got, want)
			}
		}
	})
	t.Run("IPv4 outranks every IPv6", func(t *testing.T) {
		got := OrderDialCandidates([]string{"2606:4700::1", "192.168.0.107", "fdc5::1"})
		if len(got) != 3 || got[0] != "192.168.0.107" {
			t.Fatalf("OrderDialCandidates = %v, want the IPv4 address first", got)
		}
	})
	t.Run("duplicates and unparseable entries are dropped", func(t *testing.T) {
		got := OrderDialCandidates([]string{"10.0.0.1", "10.0.0.1", "", "not-an-ip"})
		if len(got) != 1 || got[0] != "10.0.0.1" {
			t.Fatalf("OrderDialCandidates = %v, want exactly one usable address", got)
		}
	})
	t.Run("the walk is bounded", func(t *testing.T) {
		many := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"}
		if got := OrderDialCandidates(many); len(got) != MaxDialCandidates {
			t.Fatalf("OrderDialCandidates returned %d addresses, want the walk capped at %d", len(got), MaxDialCandidates)
		}
	})
	t.Run("a zoned link-local address survives", func(t *testing.T) {
		got := OrderDialCandidates([]string{"fe80::1%en0"})
		if len(got) != 1 || got[0] != "fe80::1%en0" {
			t.Fatalf("OrderDialCandidates = %v, want the zone preserved (USB link-local dials rely on it)", got)
		}
	})
}
