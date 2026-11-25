package patrolkit

import (
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwork"
	"testing"
)

func TestFreshActivationAndResetIdentity(t *testing.T) {
	now := int64(5000)
	p := Profile{RunID: "new-run", ModelSHA256: Digest([]byte("model")), PreparationBootID: "preparation"}
	sample := patrolwork.Sample{Schema: patrolwork.Schema, Method: patrolwork.Method, RunID: p.RunID, ModelSHA256: p.ModelSHA256, BootID: "activation", Sequence: 100, SimulationNS: 2000, CapturedUnixMS: now, TotalJ: 50, Valid: true}
	b, e := NewBoot(p, sample, now)
	if e != nil {
		t.Fatal(e)
	}
	// Admission starts at activation, not preparation's accumulated work.
	ledger, e := patrolwork.New(patrolwork.Policy{LimitJ: 100}, sample, now, p.RunID, p.ModelSHA256)
	if e != nil || ledger.UsedJ != 0 || ledger.Baseline.TotalJ != 50 {
		t.Fatal(ledger, e)
	}
	next := sample
	next.Sequence++
	next.SimulationNS++
	next.CapturedUnixMS++
	next.TotalJ = 52
	if e = b.Observe(&next, now+1); e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"preparation", "run", "model", "missing", "stale", "sequence-reset", "boot-reset"} {
		t.Run(mode, func(t *testing.T) {
			initial := sample
			if mode == "preparation" {
				initial.BootID = p.PreparationBootID
				if _, e := NewBoot(p, initial, now); e == nil {
					t.Fatal("preparation boot admitted")
				}
				return
			}
			state, e := NewBoot(p, initial, now)
			if e != nil {
				t.Fatal(e)
			}
			changed := next
			ptr := &changed
			checkNow := now + 1
			switch mode {
			case "run":
				changed.RunID = "other"
			case "model":
				changed.ModelSHA256 = Digest([]byte("other"))
			case "missing":
				ptr = nil
			case "stale":
				checkNow += 1001
			case "sequence-reset":
				changed.Sequence = 1
			case "boot-reset":
				changed.BootID = "restarted"
			}
			if e = state.Observe(ptr, checkNow); e == nil || state.Ready {
				t.Fatal("invalid active identity accepted")
			}
			if mode == "missing" || mode == "stale" {
				if state.Failed {
					t.Fatal("delivery gap permanently changed identity")
				}
				if e = state.Observe(&next, now+2); e != nil || !state.Ready {
					t.Fatal("same boot did not recover fresh evidence", e)
				}
			} else if e = state.Observe(&next, now+2); e == nil {
				t.Fatal("reset activation silently recovered")
			}
		})
	}
}
