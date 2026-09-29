package services

import "sync"

// AgentInstaller guards agent binary replacement and OS updates. A single
// instance is shared by the v1 and v2 handlers so no two updates run at once.
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
