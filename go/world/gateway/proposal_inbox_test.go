package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInboxConfigRefusesBeforeOpeningWriters(t *testing.T) {
	for _, kind := range []string{"no-origins", "invalid-origin", "missing-principal", "catalog-downgrade", "negative-capacity", "world-alias", "lock-alias"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			world := filepath.Join(dir, "world")
			origin := map[string]any{"principal": "gateway-a", "assignee": "robot/navigation", "run_id": "test", "catalog": map[string]string{"wendy.nav.inspect.v1": "MOVE"}}
			origins := map[string]any{"gateway-a": origin}
			inbox := map[string]any{"origins": origins}
			value := map[string]any{"allow_insecure_simulation": true, "listen": "127.0.0.1:0", "data_path": world, "tenant": "tenant", "site": "site", "audience": "reader", "root_public_key": make([]byte, 32), "clock_domain": "simulation:test", "frames": []map[string]string{{"id": "floor", "revision": "1"}}, "proposal_inbox": inbox}
			switch kind {
			case "no-origins":
				delete(inbox, "origins")
			case "invalid-origin":
				origins["bad&origin"] = origin
			case "missing-principal":
				delete(origin, "principal")
			case "catalog-downgrade":
				origin["catalog"] = map[string]string{"wendy.nav.inspect.v1": "FREE"}
			case "negative-capacity":
				inbox["max_entries"] = -1
			case "world-alias":
				inbox["path"] = world
			case "lock-alias":
				inbox["path"] = world + ".lock"
			}
			b, _ := json.Marshal(value)
			if e := os.WriteFile(path, b, 0600); e != nil {
				t.Fatal(e)
			}
			if e := run(path); e == nil {
				t.Fatal("invalid inbox accepted")
			}
			entries, e := os.ReadDir(dir)
			if e != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
				t.Fatal("invalid config opened state", entries, e)
			}
		})
	}
}
