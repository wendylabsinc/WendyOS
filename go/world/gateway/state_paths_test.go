package gateway

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

func TestStatePathsReserveJournalAndLockNamespaces(t *testing.T) {
	dir := t.TempDir()
	replay := filepath.Join(dir, "replay")
	stage := replay + authority.ReplayStagingSuffix
	config := filepath.Join(dir, "config.json")
	for _, collision := range []string{replay, replay + ".lock", stage, filepath.Join(stage, "next"), filepath.Join(stage, "owner-example"), dir} {
		if err := validateStatePaths([]string{replay, collision}, config, replay); !errors.Is(err, errStatePaths) {
			t.Fatalf("collision %q: %v", collision, err)
		}
	}
	if err := validateStatePaths([]string{replay, filepath.Join(dir, "world")}, config, replay); err != nil {
		t.Fatal("independent state refused", err)
	}
	if err := validateStatePaths([]string{replay}, filepath.Join(stage, "config.json"), replay); !errors.Is(err, errStatePaths) {
		t.Fatal("configuration inside staging accepted", err)
	}
	// The target directory does not exist: resolution must still reserve it.
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(stage, alias); err != nil {
		t.Fatal(err)
	}
	if err := validateStatePaths([]string{replay, filepath.Join(alias, "next")}, config, replay); !errors.Is(err, errStatePaths) {
		t.Fatal("dangling staging alias accepted", err)
	}
	if err := os.Symlink(replay, replay+".lock"); err != nil {
		t.Fatal(err)
	}
	if err := validateStatePaths([]string{replay}, config, replay); !errors.Is(err, errStatePaths) {
		t.Fatal("own lock aliases state", err)
	}
}

func TestJournalNamespaceConflictRefusesBeforeAnyWrite(t *testing.T) {
	for _, field := range []string{"data_path", "proof_path", "revocation_path", "task_relay", "evidence", "tasks", "diagnostics"} {
		for _, target := range []string{"", "next", "owner-example"} {
			t.Run(field+"/"+target, func(t *testing.T) {
				dir := t.TempDir()
				replay := filepath.Join(dir, "replay")
				stage := replay + authority.ReplayStagingSuffix
				conflict := stage
				if target != "" {
					conflict = filepath.Join(stage, target)
				}
				value := map[string]any{
					"allow_insecure_simulation": true, "listen": "127.0.0.1:0",
					"data_path": filepath.Join(dir, "world"), "replay_path": replay,
					"tenant": "tenant", "site": "site", "audience": "node",
					"root_public_key": make([]byte, 32), "clock_domain": "simulation:live",
				}
				if field == "tasks" {
					value["frames"] = []map[string]string{{"id": "floor", "revision": "1"}}
					value[field] = map[string]any{"path": conflict, "assignee": "robot/navigation", "executor_principal": "executor", "catalog": map[string]string{"wendy.nav.go_to.v1": "MOVE"}}
				} else if field == "task_relay" || field == "evidence" || field == "diagnostics" {
					value[field] = map[string]any{"path": conflict}
				} else {
					value[field] = conflict
				}
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				config := filepath.Join(dir, "config.json")
				if err = os.WriteFile(config, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err = run(config); err == nil {
					t.Fatal("namespace conflict accepted")
				}
				if field != "diagnostics" && !errors.Is(err, errStatePaths) {
					t.Fatal("wrong refusal", err)
				}
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
					t.Fatalf("refusal wrote state: %v %v", entries, err)
				}
				got, err := os.ReadFile(config)
				if err != nil || !reflect.DeepEqual(got, data) {
					t.Fatal("configuration changed", err)
				}
			})
		}
	}
}

func TestStatePathsRejectExistingHardLinkAlias(t *testing.T) {
	dir := t.TempDir()
	replay := filepath.Join(dir, "replay")
	world := filepath.Join(dir, "world")
	if err := os.WriteFile(replay, []byte("existing ledger"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(replay, world); err != nil {
		t.Fatal(err)
	}
	if err := validateStatePaths([]string{replay, world}, filepath.Join(dir, "config.json"), replay); !errors.Is(err, errStatePaths) {
		t.Fatal("two configured state names share an inode", err)
	}
}
