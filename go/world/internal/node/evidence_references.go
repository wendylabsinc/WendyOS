package node

import "github.com/wendylabsinc/wendy/go/world/internal/evidence"

// ReconcileTaskEvidence releases candidate media only after the task has an
// immutable terminal outcome without a result. A running task may still commit
// its candidate, and a successful result keeps its hold through the original
// blob expiry. Unknown tasks remain protected rather than treating missing
// task state as permission to discard evidence.
func (n *Node) ReconcileTaskEvidence() error {
	if n.cfg.Tasks == nil || n.cfg.Evidence == nil {
		return nil
	}
	run := n.cfg.Tasks.Catalog().RunID
	ids, err := n.cfg.Evidence.HoldIDs(evidence.HoldTask, run)
	if err != nil {
		return err
	}
	for _, id := range ids {
		record, err := n.cfg.Tasks.Get(id)
		if err != nil {
			continue
		}
		if record.Status.Phase.Terminal() && record.Result == nil {
			if err := n.cfg.Evidence.ReleaseHold(evidence.HoldTask, run, id); err != nil {
				return err
			}
		}
	}
	return nil
}
