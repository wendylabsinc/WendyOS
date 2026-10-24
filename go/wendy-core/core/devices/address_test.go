package devices

import "testing"

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
