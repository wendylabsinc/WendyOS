package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

func TestProposalConfigurationRefusesBeforeOpeningWriters(t *testing.T) {
	for _, kind := range []string{"missing-tasks", "negative-entries", "oversized-bytes", "undersized-bytes", "world", "task", "config", "replay-stage", "task-lock"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			world := filepath.Join(dir, "world")
			tasks := filepath.Join(dir, "tasks")
			replay := filepath.Join(dir, "replay")
			proposal := map[string]any{}
			value := map[string]any{"allow_insecure_simulation": true, "listen": "127.0.0.1:0", "data_path": world, "replay_path": replay, "tenant": "tenant", "site": "site", "audience": "node", "root_public_key": make([]byte, 32), "clock_domain": "simulation:live", "frames": []map[string]string{{"id": "floor", "revision": "1"}}, "tasks": map[string]any{"path": tasks, "assignee": "robot/navigation", "executor_principal": "executor", "catalog": map[string]string{"wendy.nav.go_to.v1": "MOVE"}}, "proposals": proposal}
			switch kind {
			case "missing-tasks":
				delete(value, "tasks")
			case "negative-entries":
				proposal["max_entries"] = -1
			case "oversized-bytes":
				proposal["max_bytes"] = (64 << 20) + 1
			case "undersized-bytes":
				proposal["max_bytes"] = 1024
			case "world":
				proposal["path"] = world
			case "task":
				proposal["path"] = tasks
			case "config":
				proposal["path"] = path
			case "replay-stage":
				proposal["path"] = filepath.Join(replay+authority.ReplayStagingSuffix, "next")
			case "task-lock":
				proposal["path"] = tasks + ".lock"
			}
			data, _ := json.Marshal(value)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := run(path); err == nil {
				t.Fatal("invalid proposal config accepted")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
				t.Fatal("refusal opened state writers", entries, err)
			}
		})
	}
}
