package gateway

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRejectsUnsupportedOrAmbiguousConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value map[string]any
		want  error
	}{
		{"implicit-mode", map[string]any{}, errSimulationTransport},
		{"physical-clock", map[string]any{"allow_insecure_simulation": true, "listen": "127.0.0.1:0", "data_path": "state.json", "tenant": "tenant", "site": "site", "audience": "node", "root_public_key": make([]byte, 32), "clock_domain": "physical"}, errSimulationClock},
		{"shared-path", map[string]any{"allow_insecure_simulation": true, "listen": "127.0.0.1:0", "data_path": "same.json", "proof_path": "same.json", "tenant": "tenant", "site": "site", "audience": "node", "root_public_key": make([]byte, 32), "clock_domain": "simulation:live"}, errStatePaths},
		{"shared-relay-path", map[string]any{"allow_insecure_simulation": true, "listen": "127.0.0.1:0", "data_path": "same.json", "task_relay": map[string]any{"path": "same.json"}, "tenant": "tenant", "site": "site", "audience": "node", "root_public_key": make([]byte, 32), "clock_domain": "simulation:live"}, errStatePaths},
		{"unknown-field", map[string]any{"unexpected": true}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			data, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			err = run(path)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("got %v, want nonnil error matching %v", err, tc.want)
			}
		})
	}
}

func TestTaskConfigurationRejectsUnsupportedCatalogs(t *testing.T) {
	for _, catalog := range []map[string]string{
		{"unsupported": "MOVE"},
		{"wendy.nav.go_to.v1": "OBSERVE"},
		{"wendy.nav.go_to.v1": "MOVE", "extra": "MOVE"},
	} {
		value := map[string]any{"allow_insecure_simulation": true, "listen": "127.0.0.1:0", "data_path": filepath.Join(t.TempDir(), "world.json"), "tenant": "tenant", "site": "site", "audience": "node", "root_public_key": make([]byte, 32), "clock_domain": "simulation:live", "frames": []map[string]string{{"id": "floor", "revision": "1"}}, "tasks": map[string]any{"assignee": "robot/navigation", "executor_principal": "executor", "catalog": catalog}}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err = run(path); !errors.Is(err, errUnsupportedTaskCatalog) {
			t.Fatalf("catalog was not rejected: %v", err)
		}
	}
}

func TestInspectionCatalogRequiresLocalEvidence(t *testing.T) {
	value := map[string]any{"allow_insecure_simulation": true, "listen": "127.0.0.1:0", "data_path": filepath.Join(t.TempDir(), "world.json"), "tenant": "tenant", "site": "site", "audience": "node", "root_public_key": make([]byte, 32), "clock_domain": "simulation:live", "frames": []map[string]string{{"id": "floor", "revision": "1"}}, "tasks": map[string]any{"assignee": "robot/navigation", "executor_principal": "executor", "catalog": map[string]string{"wendy.nav.inspect.v1": "MOVE"}}}
	data, _ := json.Marshal(value)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(path); !errors.Is(err, errInspectionEvidence) {
		t.Fatal(err)
	}
}
