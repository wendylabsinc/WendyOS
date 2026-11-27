package localmesh

import "time"

func virtualInventoryCount(ControlMessage) int { return 1 }

func virtualSeed(e *virtualEdge, now time.Time) {
	e.enqueue(e.sync.ReconcileBounded(now, e.credit()))
}

func virtualChange(e *virtualEdge, w SignedManifest, now time.Time) {
	// Match Node.queue: Record is attempted immediately; lack of channel
	// capacity requests a full repair on the next maintenance tick.
	for _, m := range e.sync.Record(w, now) {
		if e.credit() == 0 {
			e.retry = true
		} else {
			e.enqueue([]ControlMessage{m})
		}
	}
}

func virtualBeforeMaintenance(e *virtualEdge, now time.Time) {
	if e.retry {
		e.retry = false
		virtualSeed(e, now)
	}
}

func virtualMaintain(e *virtualEdge, now time.Time, periodic bool) {
	if periodic {
		virtualSeed(e, now)
	}
	e.enqueue(e.sync.ContinueReconcile(now, e.credit()))
}
