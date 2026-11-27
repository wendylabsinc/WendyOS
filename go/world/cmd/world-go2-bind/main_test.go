package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestArgumentsRequireExplicitPinsAndNoCheckOverrides(t *testing.T) {
	common := []string{"--package", "world.json", "--project-dir", "project", "--conversion-report", "conversion.json", "--out", "new.json"}
	if _, err := parse(append([]string{"bind"}, common...)); err == nil {
		t.Fatal("implicit runtime binding accepted")
	}
	valid := append([]string{"check", "--binding", "binding.json"}, common...)
	if _, err := parse(valid); err != nil {
		t.Fatal(err)
	}
	if _, err := parse(append(valid, "--endpoint", "http://127.0.0.1:8890")); err == nil {
		t.Fatal("check pin override accepted")
	}
	for _, phase := range []string{"apply", "reset", "resume", "move"} {
		if _, err := parse([]string{phase}); err == nil {
			t.Fatal("mutation command accepted")
		}
	}
}

func TestInvalidCheckWritesNewStructuredReport(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prior := filepath.Join(dir, "prior.json")
	if err = os.WriteFile(prior, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "result.json")
	var stdout bytes.Buffer
	args := []string{"check", "--binding", prior, "--package", "unused", "--project-dir", "unused", "--conversion-report", "unused", "--out", out}
	if err = run(context.Background(), args, &stdout); err == nil {
		t.Fatal("invalid binding succeeded")
	}
	var result map[string]any
	if err = json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["valid"] != false || result["operational_authority"] != false || result["reason"] != "binding_invalid" {
		t.Fatal(result)
	}
	saved, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err = run(context.Background(), args, &stdout); err == nil {
		t.Fatal("report overwritten")
	}
	again, _ := os.ReadFile(out)
	if !bytes.Equal(saved, again) {
		t.Fatal("immutable result changed")
	}
}
