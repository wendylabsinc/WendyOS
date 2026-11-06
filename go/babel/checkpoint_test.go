package babel

import (
	"net/netip"
	"reflect"
	"testing"
	"time"
)

func TestCheckpointPreservesDropsAcrossRepeatedRestores(t *testing.T) {
	e := newTestEngine(t, 1)
	e.cfg.MaxRoutes = 2
	step(t, e, 0, AddLink{testLink(1)})
	p := netip.MustParsePrefix("fd00::1/128")
	q := netip.MustParsePrefix("fd00::2/128")
	// Include a withdrawn prefix and a finite selected prefix. Both can still
	// have neighbours pointing at us and must survive as suppression entries.
	step(t, e, 0, Originate{Prefix: q})
	step(t, e, 0, Originate{Prefix: p})
	step(t, e, 100*time.Millisecond, Tick{})
	step(t, e, 100*time.Millisecond, Withdraw{Prefix: p})
	state := e.Checkpoint()
	if !reflect.DeepEqual(state.Drops, []netip.Prefix{p, q}) {
		t.Fatal("missing or unordered initial suppression prefixes", state.Drops)
	}
	for restart := 0; restart < 3; restart++ {
		r, err := Restore(e.cfg, state)
		if err != nil {
			t.Fatal(err)
		}
		// Persisting before the first Step is valid and must not erase history.
		next := r.Checkpoint()
		if !reflect.DeepEqual(state, next) {
			t.Fatalf("restart %d changed safety history: before=%+v after=%+v", restart, state, next)
		}
		state = next
	}
}
