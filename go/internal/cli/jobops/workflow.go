package jobops

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

// SubmitOptions folds bounded waiting and selected report retrieval into one
// operation. Collection is requested explicitly and retains the mutating host
// tool's authorization requirements.
type SubmitOptions struct {
	WaitSeconds         int
	ArtifactFiles       []string
	ArtifactDigests     []string
	ArtifactDestination string
	ReportFields        map[string][]string
	Target              string
	WorkflowSHA256      string
}

func ValidateSubmitOptions(opts SubmitOptions) error {
	if opts.WaitSeconds < 0 || opts.WaitSeconds > 3600 {
		return fmt.Errorf("wait_seconds must be 0..3600")
	}
	if len(opts.ArtifactFiles) == 0 && opts.ArtifactDestination == "" && len(opts.ReportFields) == 0 && len(opts.ArtifactDigests) == 0 {
		return nil
	}
	if opts.WaitSeconds == 0 || !filepath.IsAbs(opts.ArtifactDestination) {
		return fmt.Errorf("artifact collection requires wait_seconds > 0 and an absolute local artifact_destination")
	}
	if err := validateArtifactFiles(opts.ArtifactFiles); err != nil {
		return err
	}
	if err := validateArtifactDigests(opts.ArtifactFiles, opts.ArtifactDigests); err != nil {
		return err
	}
	return validateReportFields(opts.ArtifactFiles, opts.ReportFields)
}

// SubmitWorkflow retains the confirmed receipt on any later observation or
// collection error. A running job returns artifacts_pending without local writes.
func SubmitWorkflow(ctx context.Context, conn *grpcclient.AgentConnection, spec Spec, opts SubmitOptions) (map[string]any, error) {
	if err := ValidateSubmitOptions(opts); err != nil {
		return nil, err
	}
	receipt, err := SubmitObserved(ctx, conn, spec, opts.WaitSeconds)
	if err != nil {
		return receipt, err
	}
	if opts.WorkflowSHA256 != "" {
		receipt["workflow_sha256"] = opts.WorkflowSHA256
	}
	if len(opts.ArtifactFiles) == 0 {
		return receipt, err
	}
	obs, _ := receipt["observation"].(map[string]any)
	state, _ := obs["reported_state"].(string)
	if state != "completed" && state != "failed" && state != "stopped" && state != "cancelled" {
		receipt["artifacts_pending"] = true
		return receipt, nil
	}
	jobPath, _ := receipt["job_path"].(string)
	artifacts, err := Pull(ctx, conn, jobPath, opts.ArtifactDestination, opts.ArtifactFiles)
	if err != nil {
		receipt["artifact_error"] = err.Error()
	} else {
		receipt["artifacts"] = artifacts
	}
	if err == nil && len(opts.ReportFields) > 0 {
		reports, err := summarizeReports(opts.ArtifactDestination, artifacts, opts.ReportFields)
		if err != nil {
			receipt["report_error"] = err.Error()
		} else {
			receipt["reports"] = reports
		}
	}
	if err == nil && len(opts.ArtifactDigests) > 0 {
		digests, e := selectedArtifactDigests(opts.ArtifactDestination, artifacts, opts.ArtifactDigests)
		if e != nil {
			receipt["artifact_digest_error"] = e.Error()
		} else {
			receipt["artifact_digests"] = digests
		}
	}

	target := opts.Target
	if target == "" {
		target, _ = obs["target"].(string)
	}
	receipt["target"] = target
	if err := saveWorkflowReceipt(opts.ArtifactDestination, receipt); err != nil {
		receipt["receipt_error"] = err.Error()
	}
	return receipt, nil
}
