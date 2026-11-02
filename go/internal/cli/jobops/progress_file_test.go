package jobops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/jobops"
)

func TestDeclaredNestedMetricsAppearWithoutRawLogPolling(t *testing.T) {
	spec := jobops.Spec{Name: "nested-progress", WorkingDirectory: t.TempDir(), Steps: []jobops.Step{{Name: "train", MetricsFile: "output/metrics.jsonl", Argv: []string{"python3", "-c", `import os,pathlib;p=pathlib.Path(os.environ['WENDY_STEP_DIR'],'output');p.mkdir();(p/'metrics.jsonl').write_text('{"iteration":65,"optimizer_steps_total":16512}\n')`}, TimeoutSeconds: 2}}}
	r, err := jobops.SubmitObserved(context.Background(), connection(t, &host{}), spec, 3)
	if err != nil {
		t.Fatal(err)
	}
	progress := r["observation"].(map[string]any)["progress"].(map[string]any)
	train, ok := progress["train"].(map[string]any)
	if !ok || train["iteration"] != float64(65) || train["optimizer_steps_total"] != float64(16512) {
		t.Fatalf("nested metrics missing: %+v", r)
	}
}

func TestNestedMetricsPathCannotEscapeOwnedStep(t *testing.T) {
	for _, path := range []string{"../other.jsonl", "/outside.jsonl", ".", "a/../metrics.jsonl", "nul\x00"} {
		spec := jobops.Spec{Name: "bad-progress", WorkingDirectory: "/tmp/project", Steps: []jobops.Step{{Name: "train", MetricsFile: path, Argv: []string{"python3"}, TimeoutSeconds: 2}}}
		if _, err := jobops.Submit(context.Background(), nil, spec); err == nil || !strings.Contains(err.Error(), "metrics_file") {
			t.Fatalf("unsafe metrics path reached host: %q %v", path, err)
		}
	}
}
