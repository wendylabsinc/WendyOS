// Package container implements container health monitoring and restart policies.
package container

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/internal/agent/services"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// RestartPolicy determines the container restart behavior.
type RestartPolicy int

const (
	// RestartNo never restarts the container.
	RestartNo RestartPolicy = iota
	// RestartUnlessStopped restarts unless explicitly stopped.
	RestartUnlessStopped
	// RestartOnFailure restarts only on non-zero exit codes.
	RestartOnFailure
	// RestartAlways always restarts the container.
	RestartAlways
)

// String returns the human-readable name of the restart policy.
func (p RestartPolicy) String() string {
	switch p {
	case RestartNo:
		return "no"
	case RestartUnlessStopped:
		return "unless-stopped"
	case RestartOnFailure:
		return "on-failure"
	case RestartAlways:
		return "always"
	default:
		return fmt.Sprintf("unknown(%d)", int(p))
	}
}

// ParseRestartPolicy converts a string to a RestartPolicy.
func ParseRestartPolicy(s string) (RestartPolicy, error) {
	switch s {
	case "no", "":
		return RestartNo, nil
	case "unless-stopped":
		return RestartUnlessStopped, nil
	case "on-failure":
		return RestartOnFailure, nil
	case "always":
		return RestartAlways, nil
	default:
		return RestartNo, fmt.Errorf("unknown restart policy: %q", s)
	}
}

// containerState tracks the runtime state of a monitored container.
type containerState struct {
	FailureCount  int
	LastRestart   time.Time
	ExplicitStop  bool
	RestartPolicy RestartPolicy
	MaxRetries    int
}

// ContainerMonitor monitors container health and implements restart policies.
type ContainerMonitor struct {
	logger     *zap.Logger
	containerd services.ContainerdClient
	states     map[string]*containerState
	mu         sync.Mutex
	interval   time.Duration
	stopCh     chan struct{}
}

// NewContainerMonitor creates a new ContainerMonitor.
func NewContainerMonitor(logger *zap.Logger, client services.ContainerdClient, interval time.Duration) *ContainerMonitor {
	if interval == 0 {
		interval = 5 * time.Second
	}
	return &ContainerMonitor{
		logger:     logger,
		containerd: client,
		states:     make(map[string]*containerState),
		interval:   interval,
		stopCh:     make(chan struct{}),
	}
}

// Register registers a container for monitoring with a given restart policy.
func (m *ContainerMonitor) Register(appName string, policy RestartPolicy, maxRetries int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.states[appName] = &containerState{
		RestartPolicy: policy,
		MaxRetries:    maxRetries,
	}
	m.logger.Info("Container registered for monitoring",
		zap.String("app_name", appName),
		zap.Int("policy", int(policy)),
	)
}

// Unregister removes a container from monitoring.
func (m *ContainerMonitor) Unregister(appName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, appName)
}

// MarkExplicitStop marks a container as explicitly stopped, preventing restart.
func (m *ContainerMonitor) MarkExplicitStop(appName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state, ok := m.states[appName]; ok {
		state.ExplicitStop = true
	}
}

// Start begins the monitoring loop in a goroutine.
func (m *ContainerMonitor) Start(ctx context.Context) {
	go m.Run(ctx)
}

// Stop signals the monitor to stop.
func (m *ContainerMonitor) Stop() {
	close(m.stopCh)
}

// Run is the main monitoring loop that checks container health and restarts as needed.
func (m *ContainerMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.checkContainers(ctx)
		}
	}
}

// checkContainers queries containerd for running containers and restarts any that
// have exited according to their restart policy.
func (m *ContainerMonitor) checkContainers(ctx context.Context) {
	containers, err := m.containerd.ListContainers(ctx)
	if err != nil {
		m.logger.Error("Failed to list containers for health check", zap.Error(err))
		return
	}

	// Build a set of running container names.
	running := make(map[string]bool)
	for _, c := range containers {
		if c.GetRunningState() == agentpb.AppRunningState_RUNNING {
			running[c.GetAppName()] = true
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for appName, state := range m.states {
		if running[appName] {
			continue
		}

		// Container is not running - evaluate restart policy.
		if !m.shouldRestart(state) {
			continue
		}

		// Enforce backoff: don't restart more often than once per 10 seconds.
		if time.Since(state.LastRestart) < 10*time.Second {
			continue
		}

		m.logger.Info("Restarting container",
			zap.String("app_name", appName),
			zap.Int("failure_count", state.FailureCount),
		)

		state.FailureCount++
		state.LastRestart = time.Now()

		go func(name string) {
			if _, err := m.containerd.StartContainer(ctx, name); err != nil {
				m.logger.Error("Failed to restart container",
					zap.String("app_name", name),
					zap.Error(err),
				)
			}
		}(appName)
	}
}

// shouldRestart determines whether a container should be restarted based on its policy.
func (m *ContainerMonitor) shouldRestart(state *containerState) bool {
	switch state.RestartPolicy {
	case RestartNo:
		return false
	case RestartUnlessStopped:
		return !state.ExplicitStop
	case RestartOnFailure:
		if state.ExplicitStop {
			return false
		}
		if state.MaxRetries > 0 && state.FailureCount >= state.MaxRetries {
			return false
		}
		return true
	case RestartAlways:
		return true
	default:
		return false
	}
}
