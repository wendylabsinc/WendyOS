package localmesh

import (
	"os"
	"time"
)

// Only this adapter requires the negotiated batching implementation. The old
// base/head adapters remain buildable against commits without this capability.
func virtualBatches(e *virtualEdge) bool {
	switch os.Getenv("WENDY_MESH_HAVE_BATCHES") {
	case "0":
		return false
	case "mixed":
		return e.from%2 == 0 && e.to%2 == 0
	default:
		return true
	}
}
func virtualSeed(e *virtualEdge, now time.Time) {
	e.sync.haveBatches = virtualBatches(e)
	e.sync.seedAnnouncements(e.own, now)
	e.enqueue(e.sync.drainAnnouncements(now, e.credit()))
}
func virtualChange(e *virtualEdge, w SignedManifest, now time.Time) {
	e.sync.haveBatches = virtualBatches(e)
	e.sync.markAnnouncement(w, now)
	e.enqueue(e.sync.drainAnnouncements(now, e.credit()))
}
func virtualMaintain(e *virtualEdge, now time.Time, periodic bool) {
	e.enqueue(e.sync.drainAnnouncements(now, e.credit()))
}
func virtualBeforeMaintenance(e *virtualEdge, now time.Time) {}

func virtualInventoryCount(m ControlMessage) int {
	if m.Kind == "manifest-haves" {
		return len(m.Haves) / manifestDigestSize
	}
	return 1
}
