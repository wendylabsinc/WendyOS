//go:build world_simulation_faults

package authority

import (
	"errors"
	"time"
)

// SetCommitDelayForSimulation is available only in explicitly fault-tagged
// builds. The caller must validate its complete configuration is simulation.
// This delays every attempted durable commit; zero disables the injection.
func (c *ReplayCache) SetCommitDelayForSimulation(delay time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.poisoned || c.path == "" || delay < 0 || delay > time.Second {
		return errors.New("invalid simulation replay commit delay")
	}
	c.beforeCommit = nil
	if delay > 0 {
		c.beforeCommit = func() { time.Sleep(delay) }
	}
	return nil
}
