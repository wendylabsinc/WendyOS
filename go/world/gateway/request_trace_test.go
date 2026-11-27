package gateway

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/diagnostics"
)

func TestTruncatedNodeConfigurationPreservesDecodeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":`), 0600); err != nil {
		t.Fatal(err)
	}
	err := run(path)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("configuration context lost the decoder error", err)
	}
}

func TestRequestTracePathsCannotReplaceStateOrAnotherDiagnostic(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	stage := filepath.Join(dir, "replay") + authority.ReplayStagingSuffix
	metrics := &diagnosticsConfig{Path: filepath.Join(dir, "metrics.json")}
	trace := &diagnostics.Config{Path: filepath.Join(dir, "trace.json"), TriggerPath: filepath.Join(dir, "trigger.json")}
	protected := []string{state, stage, filepath.Join(dir, "config.json")}
	if err := validateDiagnosticPaths(metrics, trace, protected); err != nil {
		t.Fatal(err)
	}
	if err := validateDiagnosticPaths(nil, nil, protected); err != nil {
		t.Fatal("disabled diagnostics rejected", err)
	}
	for _, target := range []string{"", state, state + ".lock", filepath.Join(stage, "next"), metrics.Path, metrics.Path + ".lock", dir} {
		copy := *trace
		copy.Path = target
		if err := validateDiagnosticPaths(metrics, &copy, protected); err == nil {
			t.Fatal("unsafe trace output accepted", target)
		}
		copy = *trace
		copy.TriggerPath = target
		if err := validateDiagnosticPaths(metrics, &copy, protected); err == nil {
			t.Fatal("unsafe trace trigger accepted", target)
		}
	}
	for _, target := range []string{trace.TriggerPath, trace.TriggerPath + ".lock"} {
		copy := *trace
		copy.Path = target
		if err := validateDiagnosticPaths(metrics, &copy, protected); err == nil {
			t.Fatal("trace output collides with trigger", target)
		}
	}
	for _, target := range []string{trace.Path, trace.Path + ".lock"} {
		copy := *trace
		copy.TriggerPath = target
		if err := validateDiagnosticPaths(metrics, &copy, protected); err == nil {
			t.Fatal("trigger collides with trace output", target)
		}
	}
	for _, target := range []string{trace.Path, trace.Path + ".lock", trace.TriggerPath, trace.TriggerPath + ".lock"} {
		if err := validateDiagnosticPaths(&diagnosticsConfig{Path: target}, trace, protected); err == nil {
			t.Fatal("metrics output may replace trace namespace", target)
		}
	}
}

func TestRequestTraceRejectsFutureAliasedStateAndTrigger(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "future")
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	trace := &diagnostics.Config{Path: filepath.Join(dir, "trace.json"), TriggerPath: filepath.Join(alias, "state.lock")}
	if err := validateDiagnosticPaths(nil, trace, []string{filepath.Join(real, "state")}); err == nil {
		t.Fatal("aliased future lock accepted as trigger")
	}
	trace.Path, trace.TriggerPath = filepath.Join(alias, "same.json"), filepath.Join(real, "same.json")
	if err := validateDiagnosticPaths(nil, trace, nil); err == nil {
		t.Fatal("trace and trigger alias accepted")
	}
}
