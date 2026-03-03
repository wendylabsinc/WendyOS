package container

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestRestartPolicy_String(t *testing.T) {
	tests := []struct {
		policy RestartPolicy
		want   string
	}{
		{RestartNo, "no"},
		{RestartUnlessStopped, "unless-stopped"},
		{RestartOnFailure, "on-failure"},
		{RestartAlways, "always"},
		{RestartPolicy(99), "unknown(99)"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := tt.policy.String()
			if got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseRestartPolicy(t *testing.T) {
	tests := []struct {
		input   string
		want    RestartPolicy
		wantErr bool
	}{
		{"no", RestartNo, false},
		{"", RestartNo, false},
		{"unless-stopped", RestartUnlessStopped, false},
		{"on-failure", RestartOnFailure, false},
		{"always", RestartAlways, false},
		{"invalid", RestartNo, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseRestartPolicy(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseRestartPolicy(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ParseRestartPolicy(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func newTestMonitor() *ContainerMonitor {
	logger := zap.NewNop()
	return NewContainerMonitor(logger, nil, 1*time.Second)
}

func TestContainerMonitor_ShouldRestart_No(t *testing.T) {
	m := newTestMonitor()
	state := &containerState{
		RestartPolicy: RestartNo,
	}

	if m.shouldRestart(state) {
		t.Error("shouldRestart() = true for RestartNo, want false")
	}
}

func TestContainerMonitor_ShouldRestart_UnlessStopped(t *testing.T) {
	m := newTestMonitor()

	// Should restart when not explicitly stopped.
	state := &containerState{
		RestartPolicy: RestartUnlessStopped,
		ExplicitStop:  false,
	}
	if !m.shouldRestart(state) {
		t.Error("shouldRestart() = false for UnlessStopped (not stopped), want true")
	}

	// Should not restart when explicitly stopped.
	state.ExplicitStop = true
	if m.shouldRestart(state) {
		t.Error("shouldRestart() = true for UnlessStopped (explicitly stopped), want false")
	}
}

func TestContainerMonitor_ShouldRestart_OnFailure(t *testing.T) {
	m := newTestMonitor()

	// Should restart when under max retries.
	state := &containerState{
		RestartPolicy: RestartOnFailure,
		MaxRetries:    3,
		FailureCount:  1,
	}
	if !m.shouldRestart(state) {
		t.Error("shouldRestart() = false for OnFailure (under max retries), want true")
	}

	// Should not restart when at max retries.
	state.FailureCount = 3
	if m.shouldRestart(state) {
		t.Error("shouldRestart() = true for OnFailure (at max retries), want false")
	}

	// Should not restart when explicitly stopped.
	state.FailureCount = 0
	state.ExplicitStop = true
	if m.shouldRestart(state) {
		t.Error("shouldRestart() = true for OnFailure (explicitly stopped), want false")
	}

	// Zero max retries means unlimited retries.
	stateUnlimited := &containerState{
		RestartPolicy: RestartOnFailure,
		MaxRetries:    0,
		FailureCount:  100,
	}
	if !m.shouldRestart(stateUnlimited) {
		t.Error("shouldRestart() = false for OnFailure (unlimited retries), want true")
	}
}

func TestContainerMonitor_ExplicitStop(t *testing.T) {
	m := newTestMonitor()

	m.Register("test-app", RestartUnlessStopped, 0)

	// Mark as explicitly stopped.
	m.MarkExplicitStop("test-app")

	m.mu.Lock()
	state, ok := m.states["test-app"]
	m.mu.Unlock()

	if !ok {
		t.Fatal("test-app not found in states")
	}
	if !state.ExplicitStop {
		t.Error("ExplicitStop = false after MarkExplicitStop, want true")
	}

	// Should not restart.
	if m.shouldRestart(state) {
		t.Error("shouldRestart() = true after explicit stop, want false")
	}
}

func TestContainerMonitor_Register_And_Unregister(t *testing.T) {
	m := newTestMonitor()

	m.Register("app-1", RestartAlways, 0)
	m.Register("app-2", RestartOnFailure, 5)

	m.mu.Lock()
	if len(m.states) != 2 {
		t.Errorf("states count = %d, want 2", len(m.states))
	}
	m.mu.Unlock()

	m.Unregister("app-1")

	m.mu.Lock()
	if len(m.states) != 1 {
		t.Errorf("states count after unregister = %d, want 1", len(m.states))
	}
	if _, ok := m.states["app-1"]; ok {
		t.Error("app-1 still in states after Unregister")
	}
	m.mu.Unlock()
}
