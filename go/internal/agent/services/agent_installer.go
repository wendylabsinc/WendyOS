package services

import "sync"

// AgentInstaller guards agent binary replacement. A single instance is shared
// between the v1 AgentService and the v2 AgentUpdateService so that concurrent
// update calls from different API versions cannot race on the same executable.
type AgentInstaller struct {
	mu         sync.Mutex
	isUpdating bool
}

// TryLock marks an update as in progress. It returns true if the lock was
// acquired and false if an update is already running.
func (i *AgentInstaller) TryLock() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.isUpdating {
		return false
	}
	i.isUpdating = true
	return true
}

// Unlock releases the update lock.
func (i *AgentInstaller) Unlock() {
	i.mu.Lock()
	i.isUpdating = false
	i.mu.Unlock()
}
