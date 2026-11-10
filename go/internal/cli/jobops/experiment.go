package jobops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// ExperimentRecipe expands project-owned argv templates, never executable
// template code. Source/target/budget decisions remain explicit in the project.
type ExperimentRecipe struct {
	Schema      string              `json:"schema"`
	Parameters  map[string][]string `json:"parameters"`
	BeforeSteps []Step              `json:"before_steps,omitempty"`
	AfterSteps  []Step              `json:"after_steps,omitempty"`
	Workflow    WorkflowFile        `json:"workflow"`
}

var parameterName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
var parameterToken = regexp.MustCompile(`\{\{([a-z][a-z0-9_]{0,31})\}\}`)

func compileExperiment(data []byte) (WorkflowFile, int, error) {
	var recipe ExperimentRecipe
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&recipe); err != nil {
		return WorkflowFile{}, 0, fmt.Errorf("invalid recipe: %w", err)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || recipe.Schema != "wendy.experiment.v1" || recipe.Workflow.Schema != "wendy.workflow.v1" {
		return WorkflowFile{}, 0, fmt.Errorf("expected one wendy.experiment.v1 recipe containing a wendy.workflow.v1 workflow")
	}
	if len(recipe.Parameters) == 0 || len(recipe.Parameters) > 4 || len(recipe.Workflow.Job.Steps) == 0 {
		return WorkflowFile{}, 0, fmt.Errorf("recipe requires 1..4 parameter axes and at least one repeated step")
	}
	keys := make([]string, 0, len(recipe.Parameters))
	for key, values := range recipe.Parameters {
		if !parameterName.MatchString(key) || len(values) == 0 || len(values) > 32 {
			return WorkflowFile{}, 0, fmt.Errorf("invalid parameter axis %q", key)
		}
		seen := map[string]bool{}
		for _, value := range values {
			if len(value) == 0 || len(value) > 160 || strings.ContainsAny(value, "\x00{}") || seen[value] {
				return WorkflowFile{}, 0, fmt.Errorf("parameter values must be unique nonempty strings, at most 160 bytes, without braces or NUL")
			}
			seen[value] = true
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	variants := []map[string]string{{}}
	for _, key := range keys {
		if len(variants)*len(recipe.Parameters[key]) > 32 {
			return WorkflowFile{}, 0, fmt.Errorf("parameter product exceeds 32 variants")
		}
		var next []map[string]string
		for _, prior := range variants {
			for _, value := range recipe.Parameters[key] {
				v := map[string]string{}
				for k, x := range prior {
					v[k] = x
				}
				v[key] = value
				next = append(next, v)
			}
		}
		variants = next
	}
	if len(recipe.BeforeSteps)+len(variants)*len(recipe.Workflow.Job.Steps)+len(recipe.AfterSteps) > 32 {
		return WorkflowFile{}, 0, fmt.Errorf("expanded recipe exceeds 32 steps; split into separate explicit jobs")
	}
	w := recipe.Workflow
	w.Job.Steps = slices.Clone(recipe.BeforeSteps)
	w.ArtifactFiles = nil
	w.ArtifactDigests = nil
	w.ReportFields = map[string][]string{}
	used := map[string]bool{}
	var expansionErr error
	expand := func(text string, values map[string]string) string {
		out := parameterToken.ReplaceAllStringFunc(text, func(token string) string {
			key := token[2 : len(token)-2]
			v, ok := values[key]
			if !ok {
				expansionErr = fmt.Errorf("unknown parameter %q", key)
			}
			used[key] = true
			return v
		})
		if strings.Contains(out, "{{") {
			expansionErr = fmt.Errorf("unresolved parameter token")
		}
		return out
	}
	for _, values := range variants {
		for _, template := range recipe.Workflow.Job.Steps {
			s := template
			s.Name = expand(s.Name, values)
			s.MetricsFile = expand(s.MetricsFile, values)
			s.Argv = slices.Clone(s.Argv)
			for i := range s.Argv {
				s.Argv[i] = expand(s.Argv[i], values)
			}
			w.Job.Steps = append(w.Job.Steps, s)
		}
		for _, name := range recipe.Workflow.ArtifactFiles {
			name = expand(name, values)
			if !slices.Contains(w.ArtifactFiles, name) {
				w.ArtifactFiles = append(w.ArtifactFiles, name)
			}
		}
		for _, name := range recipe.Workflow.ArtifactDigests {
			name = expand(name, values)
			if !slices.Contains(w.ArtifactDigests, name) {
				w.ArtifactDigests = append(w.ArtifactDigests, name)
			}
		}
		for name, pointers := range recipe.Workflow.ReportFields {
			name = expand(name, values)
			fields := make([]string, len(pointers))
			for i, p := range pointers {
				fields[i] = expand(p, values)
			}
			if prior, exists := w.ReportFields[name]; exists && !slices.Equal(prior, fields) {
				return WorkflowFile{}, 0, fmt.Errorf("report projection collision for %q", name)
			}
			w.ReportFields[name] = fields
		}
	}
	w.Job.Steps = append(w.Job.Steps, recipe.AfterSteps...)
	if expansionErr != nil {
		return WorkflowFile{}, 0, expansionErr
	}
	for _, key := range keys {
		if !used[key] {
			return WorkflowFile{}, 0, fmt.Errorf("unused parameter %q", key)
		}
	}
	// Tokens are permitted only in the repeated steps and evidence selections.
	// Never parameterize source digests, job identity, target or working directory.
	encoded, _ := json.Marshal(w)
	if bytes.Contains(encoded, []byte("{{")) {
		return WorkflowFile{}, 0, fmt.Errorf("parameter tokens outside repeated steps/evidence are forbidden")
	}
	if len(encoded)+1 > 120000 {
		return WorkflowFile{}, 0, fmt.Errorf("expanded workflow exceeds 120000 bytes")
	}
	if err := ValidateSpec(w.Job); err != nil {
		return WorkflowFile{}, 0, err
	}
	if err := ValidateSubmitOptions(SubmitOptions{WaitSeconds: w.WaitSeconds, ArtifactFiles: w.ArtifactFiles, ArtifactDigests: w.ArtifactDigests, ArtifactDestination: w.ArtifactDestination, ReportFields: w.ReportFields}); err != nil {
		return WorkflowFile{}, 0, err
	}
	return w, len(variants), nil
}

// PlanExperiment saves a new local workflow; it does not connect to a device,
// validate a Python runtime, inspect source bytes or execute any declared argv.
func PlanExperiment(recipePath, destination string) (map[string]any, error) {
	if !filepath.IsAbs(recipePath) || !filepath.IsAbs(destination) {
		return nil, fmt.Errorf("recipe_path and destination must be absolute local paths")
	}
	data, err := readLocalSpec(recipePath)
	if err != nil {
		return nil, err
	}
	w, variants, err := compileExperiment(data)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	f, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if _, err = f.Write(encoded); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(destination)
		return nil, err
	}
	seconds := 0
	for _, s := range w.Job.Steps {
		seconds += s.TimeoutSeconds
	}
	return map[string]any{"workflow_path": destination, "workflow_sha256": digest(encoded), "recipe_sha256": digest(data), "variants": variants, "steps": len(w.Job.Steps), "declared_step_timeout_sum_seconds": seconds, "validation": "structure_only", "execution_started": false, "source_integrity": "not_checked"}, nil
}
