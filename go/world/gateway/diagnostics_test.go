package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

func TestDiagnosticsCannotUseConfigOrStateNamespace(t *testing.T) {
	dir := t.TempDir()
	protected := []string{filepath.Join(dir, "config.json"), filepath.Join(dir, "state"), filepath.Join(dir, "evidence")}
	for _, p := range []string{"", protected[0], protected[1] + ".lock", protected[2] + ".lock", filepath.Join(dir, ".", "state"), filepath.Join(dir, "evidence", "metrics.json")} {
		if err := validateDiagnosticsPath(p, protected); err == nil {
			t.Fatal("diagnostics may overwrite state", p)
		}
	}
	if err := validateDiagnosticsPath(filepath.Join(dir, "metrics.json"), protected); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticsRejectJournalStagingAliases(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, "replay") + authority.ReplayStagingSuffix
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(stage, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{stage, filepath.Join(alias, "next"), filepath.Join(alias, "owner-example"), dir} {
		if err := validateDiagnosticsPath(path, []string{stage}); err == nil {
			t.Fatal("journal staging alias accepted", path)
		}
	}
}

func TestDiagnosticsRejectAliasedAndFutureLockFiles(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"replica", "replay", "tasks", "relay"} {
		if err := validateDiagnosticsPath(filepath.Join(alias, state+".lock"), []string{filepath.Join(real, state)}); err == nil {
			t.Fatal("lock alias accepted", state)
		}
	}
	future := filepath.Join(real, "future")
	dangling := filepath.Join(dir, "future-alias")
	if err := os.Symlink(future, dangling); err != nil {
		t.Fatal(err)
	}
	if err := validateDiagnosticsPath(filepath.Join(dangling, "tasks.lock"), []string{filepath.Join(future, "tasks")}); err == nil {
		t.Fatal("future lock alias accepted")
	}
	if err := validateDiagnosticsPath(filepath.Join(alias, "metrics.json"), []string{filepath.Join(real, "tasks")}); err != nil {
		t.Fatal("separate metrics path rejected", err)
	}
	subdir := filepath.Join(real, "nested")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	nestedAlias := filepath.Join(dir, "nested-alias")
	if err := os.Symlink(subdir, nestedAlias); err != nil {
		t.Fatal(err)
	}
	if err := validateDiagnosticsPath(nestedAlias+"/../tasks.lock", []string{filepath.Join(real, "tasks")}); err == nil {
		t.Fatal("symlink parent traversal bypassed lock exclusion")
	}
	if err := validateDiagnosticsPath(filepath.Join(real, "metrics.json"), []string{nestedAlias + "/../tasks"}); err == nil {
		t.Fatal("ambiguous protected path accepted")
	}
}
