package jobops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func recipeFixture() ExperimentRecipe {
	return ExperimentRecipe{Schema: "wendy.experiment.v1", Parameters: map[string][]string{"seed": {"11", "13"}, "condition": {"control", "reward"}},
		BeforeSteps: []Step{{Name: "preflight", Argv: []string{"python3", "preflight.py"}, TimeoutSeconds: 30}},
		AfterSteps:  []Step{{Name: "compare", Argv: []string{"python3", "compare.py", "{job_dir}"}, TimeoutSeconds: 15}},
		Workflow:    WorkflowFile{Schema: "wendy.workflow.v1", Job: Spec{Name: "reward-pair", WorkingDirectory: "/owned/project", SourceHashes: map[string]string{"worker.py": strings.Repeat("a", 64)}, Steps: []Step{{Name: "eval-{{condition}}-{{seed}}", Argv: []string{"python3", "worker.py", "--seed", "{{seed}}", "--condition", "{{condition}}", "--out", "{step_dir}", "--literal", "$(touch unwanted)", "--json", `{"nested":{}}`}, TimeoutSeconds: 60}}}, WaitSeconds: 300, ArtifactDestination: "/local/reports", ArtifactFiles: []string{"status.json", "eval-{{condition}}-{{seed}}/result.json"}, ArtifactDigests: []string{"status.json"}, ReportFields: map[string][]string{"eval-{{condition}}-{{seed}}/result.json": {"/successes"}}}}
}

func TestExperimentPlanPreservesBudgetSourcesArgvAndDeterministicSeedPairs(t *testing.T) {
	r := recipeFixture()
	raw, _ := json.Marshal(r)
	w, n, err := compileExperiment(raw)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 || len(w.Job.Steps) != 6 || len(w.ArtifactFiles) != 5 || len(w.ReportFields) != 4 || w.Job.Steps[0].Name != "preflight" || w.Job.Steps[5].Name != "compare" {
		t.Fatalf("lost grid or evidence: %+v", w)
	}
	for i, name := range []string{"eval-control-11", "eval-control-13", "eval-reward-11", "eval-reward-13"} {
		if w.Job.Steps[i+1].Name != name || w.Job.Steps[i+1].TimeoutSeconds != 60 || w.Job.Steps[i+1].Argv[7] != "{step_dir}" || w.Job.Steps[i+1].Argv[9] != "$(touch unwanted)" {
			t.Fatalf("changed argv or order: %+v", w.Job.Steps[i+1])
		}
	}
	if !reflect.DeepEqual(w.Job.SourceHashes, r.Workflow.Job.SourceHashes) {
		t.Fatal("source contract changed")
	}
	for i := 0; i < 5; i++ {
		again, _, err := compileExperiment(raw)
		if err != nil || !reflect.DeepEqual(w, again) {
			t.Fatal("nondeterministic plan")
		}
	}
	dir := t.TempDir()
	recipe := filepath.Join(dir, "recipe.json")
	output := filepath.Join(dir, "workflow.json")
	os.WriteFile(recipe, raw, 0600)
	p, err := PlanExperiment(recipe, output)
	if err != nil {
		t.Fatal(err)
	}
	if p["execution_started"] != false || p["source_integrity"] != "not_checked" || p["declared_step_timeout_sum_seconds"] != 285 {
		t.Fatalf("false execution/source claim or budget: %+v", p)
	}
	if _, _, err := LoadWorkflow(output); err != nil {
		t.Fatalf("saved invalid workflow: %v", err)
	}
	before, _ := os.ReadFile(output)
	if _, err := PlanExperiment(recipe, output); err == nil {
		t.Fatal("overwrote workflow")
	}
	after, _ := os.ReadFile(output)
	if string(before) != string(after) {
		t.Fatal("changed existing output")
	}
	if _, err := os.Stat(filepath.Join(dir, "unwanted")); !os.IsNotExist(err) {
		t.Fatal("executed recipe")
	}
}

func TestExperimentRejectsAmbiguityAndOverflowWithoutCreatingOutput(t *testing.T) {
	for name, change := range map[string]func(*ExperimentRecipe){
		"unknown":        func(r *ExperimentRecipe) { r.Workflow.Job.Steps[0].Argv[1] = "{{unknown}}" },
		"unused":         func(r *ExperimentRecipe) { r.Parameters["unused"] = []string{"x"} },
		"duplicates":     func(r *ExperimentRecipe) { r.Parameters["seed"] = []string{"11", "11"} },
		"step_collision": func(r *ExperimentRecipe) { r.Workflow.Job.Steps[0].Name = "eval" },
		"unsafe_output":  func(r *ExperimentRecipe) { r.Workflow.ArtifactFiles = append(r.Workflow.ArtifactFiles, "../outside") },
		"source_token": func(r *ExperimentRecipe) {
			r.Workflow.Job.SourceHashes = map[string]string{"{{seed}}.py": strings.Repeat("a", 64)}
		},
		"working_directory_token": func(r *ExperimentRecipe) { r.Workflow.Job.WorkingDirectory = "/owned/{{seed}}" },
		"grid_overflow": func(r *ExperimentRecipe) {
			r.Parameters["large"] = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
		},
		"step_overflow": func(r *ExperimentRecipe) {
			for i := 0; i < 30; i++ {
				r.BeforeSteps = append(r.BeforeSteps, Step{Name: fmt.Sprintf("before-%d", i), Argv: []string{"true"}, TimeoutSeconds: 1})
			}
		},
		"report_overflow":      func(r *ExperimentRecipe) { r.Parameters["seed"] = []string{"1", "2", "3", "4", "5"} },
		"nested_tokens":        func(r *ExperimentRecipe) { r.Parameters["seed"] = []string{"{{condition}}"} },
		"projection_collision": func(r *ExperimentRecipe) { r.Workflow.ReportFields = map[string][]string{"status.json": {"/{{seed}}"}} },
	} {
		t.Run(name, func(t *testing.T) {
			r := recipeFixture()
			change(&r)
			raw, _ := json.Marshal(r)
			dir := t.TempDir()
			input := filepath.Join(dir, "recipe.json")
			output := filepath.Join(dir, "output.json")
			os.WriteFile(input, raw, 0600)
			if _, err := PlanExperiment(input, output); err == nil {
				t.Fatal("accepted invalid recipe")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("invalid recipe created output")
			}
		})
	}
	for _, raw := range []string{`{"schema":"wendy.experiment.v1","unknown":true}`, `{} {}`} {
		if _, _, err := compileExperiment([]byte(raw)); err == nil {
			t.Fatal("accepted unknown or trailing input")
		}
	}
}
