package jobops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// WorkflowFile is a versioned project-owned execution and evidence contract.
// It contains no target override; the explicit CLI/MCP connection selects that.
type WorkflowFile struct {
	Schema              string              `json:"schema"`
	Job                 Spec                `json:"job"`
	WaitSeconds         int                 `json:"wait_seconds"`
	ArtifactFiles       []string            `json:"artifact_files,omitempty"`
	ArtifactDigests     []string            `json:"artifact_digests,omitempty"`
	ArtifactDestination string              `json:"artifact_destination,omitempty"`
	ReportFields        map[string][]string `json:"report_fields,omitempty"`
}

func LoadWorkflow(name string) (Spec, SubmitOptions, error) {
	var workflow WorkflowFile
	data, err := readLocalSpec(name)
	if err != nil {
		return Spec{}, SubmitOptions{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&workflow); err != nil {
		return Spec{}, SubmitOptions{}, fmt.Errorf("invalid workflow: %w", err)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return Spec{}, SubmitOptions{}, fmt.Errorf("workflow must contain exactly one JSON object")
	}
	if workflow.Schema != "wendy.workflow.v1" {
		return Spec{}, SubmitOptions{}, fmt.Errorf("workflow schema must be wendy.workflow.v1")
	}
	opts := SubmitOptions{WaitSeconds: workflow.WaitSeconds, ArtifactFiles: workflow.ArtifactFiles, ArtifactDigests: workflow.ArtifactDigests, ArtifactDestination: workflow.ArtifactDestination, ReportFields: workflow.ReportFields, WorkflowSHA256: digest(data)}
	if err = ValidateSpec(workflow.Job); err != nil {
		return Spec{}, SubmitOptions{}, err
	}
	if err = ValidateSubmitOptions(opts); err != nil {
		return Spec{}, SubmitOptions{}, err
	}
	return workflow.Job, opts, nil
}
