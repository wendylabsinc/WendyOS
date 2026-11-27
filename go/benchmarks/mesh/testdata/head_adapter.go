package localmesh

import "time"

func virtualSeed(e *virtualEdge, now time.Time) {
	e.sync.seedAnnouncements(e.own, now)
	e.enqueue(e.sync.drainAnnouncements(now, e.credit()))
}

func virtualChange(e *virtualEdge, w SignedManifest, now time.Time) {
	e.sync.markAnnouncement(w, now)
	e.enqueue(e.sync.drainAnnouncements(now, e.credit()))
}

func virtualMaintain(e *virtualEdge, now time.Time, periodic bool) {
	e.enqueue(e.sync.drainAnnouncements(now, e.credit()))
}

func virtualBeforeMaintenance(e *virtualEdge, now time.Time) {}

func virtualInventoryCount(ControlMessage) int { return 1 }
