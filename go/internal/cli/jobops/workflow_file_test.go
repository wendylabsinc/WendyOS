package jobops_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/jobops"
)

func TestWorkflowFilePinsJobAndEvidenceWithoutTargetOverride(t *testing.T) {
	root := t.TempDir()
	workflow := jobops.WorkflowFile{Schema: "wendy.workflow.v1", Job: jobops.Spec{Name: "recipe", WorkingDirectory: root, Steps: []jobops.Step{{Name: "check", Argv: []string{"python3"}, TimeoutSeconds: 2}}}, WaitSeconds: 2, ArtifactFiles: []string{"result.json"}, ArtifactDestination: filepath.Join(root, "reports"), ReportFields: map[string][]string{"result.json": {"/successes"}}}
	data, _ := json.Marshal(workflow)
	name := filepath.Join(root, "workflow.json")
	os.WriteFile(name, data, 0600)
	spec, opts, err := jobops.LoadWorkflow(name)
	if err != nil || spec.Name != "recipe" || opts.WorkflowSHA256 == "" || opts.ReportFields["result.json"][0] != "/successes" || opts.Target != "" {
		t.Fatalf("workflow changed or target invented: %v %+v %+v", err, spec, opts)
	}
	for _, bad := range [][]byte{append(append([]byte{}, data...), []byte(" {}")...), []byte(`{"schema":"unsupported"}`), []byte(`{"schema":"wendy.workflow.v1","target":"cloud://other"}`)} {
		os.WriteFile(name, bad, 0600)
		if _, _, err = jobops.LoadWorkflow(name); err == nil {
			t.Fatal("accepted invalid workflow")
		}
	}
}
