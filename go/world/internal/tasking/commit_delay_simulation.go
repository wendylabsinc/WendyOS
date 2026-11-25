//go:build world_simulation_faults

package tasking

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"time"
)

// SetCommitDelayForSimulation exists only in fault-tagged builds and refuses
// physical configurations. Zero disables injection; it never extends a lease.
func (s *Store) SetCommitDelayForSimulation(delay time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return err
	}
	if s.cfg.Domain != mission.Simulation || delay < 0 || delay > time.Second {
		return errors.New("invalid simulation task commit delay")
	}
	s.beforeCommit = nil
	if delay > 0 {
		s.beforeCommit = func() { time.Sleep(delay) }
	}
	return nil
}
