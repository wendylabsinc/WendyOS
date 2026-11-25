package authority

import (
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"time"
)

// lock observes only elapsed mutex acquisition time. The fixed metric contains
// no operation, principal, proof, nonce or path labels.
func (v *Verifier) lock() {
	if v.cfg.Metrics == nil {
		v.mu.Lock()
		return
	}
	started := time.Now()
	v.mu.Lock()
	v.cfg.Metrics.Observe(runtimeobs.AuthorityLockWait, time.Since(started))
}

// SetMetrics attaches optional in-memory measurement. It neither changes
// admission state nor creates a telemetry file. Nil disables observation.
func (c *ReplayCache) SetMetrics(metrics *runtimeobs.Collector) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metrics = metrics
}

// replay.checkpoint_commit measures durable commit attempts from nonce Consume,
// combined admission, and revocation/clock Checkpoint operations. A commit may
// append and sync a journal frame or replace the journal with a compacted base.
// It includes preceding encoding and any explicitly tagged injected delay.
// Pre-write refusals are not attempts; a nonce denial can still persist a changed
// checkpoint and therefore count as a durable attempt.
// replay.checkpoint_noops counts Checkpoint calls with unchanged revision,
// digest and millisecond clock floor. Memory replay updates are not disk work.
