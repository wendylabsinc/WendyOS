package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInvalidReplicaPolicyFailsBeforeCreatingRuntimeState(t *testing.T) {
	for _, policy := range []map[string]any{{"version": 1, "tenant": "other", "site": "office", "selection": map[string]any{"types": []string{"gas"}}, "required_types": []string{"wendy.site.georeference.v1"}}, {"version": 1, "tenant": "tenant", "site": "office", "selection": map[string]any{"types": []string{"gas"}}, "required_types": []string{}}, {"version": 1, "tenant": "tenant", "site": "office", "selection": map[string]any{}, "required_types": []string{"wendy.site.georeference.v1"}}} {
		dir := t.TempDir()
		state := filepath.Join(dir, "world.json")
		value := map[string]any{"allow_insecure_simulation": true, "listen": "127.0.0.1:0", "data_path": state, "tenant": "tenant", "site": "office", "audience": "node", "root_public_key": make([]byte, 32), "clock_domain": "simulation:live", "replica_policy": policy}
		raw, _ := json.Marshal(value)
		path := filepath.Join(dir, "config.json")
		os.WriteFile(path, raw, 0600)
		if err := run(path); err == nil {
			t.Fatal("invalid policy accepted")
		}
		for _, suffix := range []string{"", ".proofs", ".revocations", ".replay"} {
			if _, err := os.Stat(state + suffix); !os.IsNotExist(err) {
				t.Fatal("invalid policy touched runtime state", suffix, err)
			}
		}
	}
}
