package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/jobops"
)

func (s *mcpServer) registerJobTools(srv *server.MCPServer) {
	plan := []mcpgo.ToolOption{mcpgo.WithDescription("Expand a project-owned wendy.experiment.v1 parameter recipe into a new local workflow. Saves deterministic bounded argv steps and selected evidence; no device access or execution. Does not verify runtime/source compatibility. Refuses overwrites. Submit the saved workflow separately when authorized."), mcpgo.WithString("recipe_path", mcpgo.Required(), mcpgo.Description("Absolute local recipe JSON path")), mcpgo.WithString("destination", mcpgo.Required(), mcpgo.Description("Absolute new local workflow file; parent directory must exist"))}
	plan = append(plan, mutating()...)
	plan = append(plan, localOnly()...)
	srv.AddTool(mcpgo.NewTool("experiment_plan", plan...), s.handleExperimentPlan)
	opts := []mcpgo.ToolOption{mcpgo.WithDescription("Read an existing host training job: bounded progress, process identity, freshness, errors and full-report reference. Requires authenticated Linux host access, python3 and status.json in job_path. Never changes a job. Use since and wait_seconds to avoid repeated full snapshots."), mcpgo.WithString("job_path", mcpgo.Required(), mcpgo.Description("Absolute directory containing status.json and optional child metrics.jsonl files")), mcpgo.WithString("since", mcpgo.Description("Cursor from a previous result")), mcpgo.WithInteger("wait_seconds", mcpgo.Min(0), mcpgo.Max(30), mcpgo.Description("Wait for a changed snapshot; requires since. Omitted: 0."))}
	opts = append(opts, readOnly()...)
	opts = append(opts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("job_inspect", opts...), s.handleJobInspect)
	submit := []mcpgo.ToolOption{mcpgo.WithDescription("Start a named durable host job. Supply exactly one of spec_path (local JSON file) or spec (inline). Runs argv with host-agent permissions; commands may write elsewhere. Identical retries reuse the job; different specs never restart it. Optional wait_seconds waits for terminal evidence without cancelling on expiry. Optional artifact_files and artifact_destination retrieve verified reports locally after terminal evidence; later errors preserve the receipt. Receipt alone is not success."), mcpgo.WithString("spec_path", mcpgo.Description("Absolute local JSON spec path on the MCP server machine, not a remote path")), mcpgo.WithInteger("wait_seconds", mcpgo.Min(0), mcpgo.Max(3600), mcpgo.Description("Wait up to 3600 seconds for terminal evidence using short host RPCs; client tool timeout must exceed this plus submission/collection time. Omitted: 0.")), mcpgo.WithObject("spec", mcpgo.Properties(map[string]any{
		"name":              map[string]any{"type": "string", "description": "Unique job name; lowercase letters, digits, underscore or hyphen"},
		"working_directory": map[string]any{"type": "string", "description": "Absolute existing remote source directory"},
		"steps":             map[string]any{"type": "array", "minItems": 1, "maxItems": 32, "items": map[string]any{"type": "object", "required": []string{"name", "argv", "timeout_seconds"}, "additionalProperties": false, "properties": map[string]any{"name": map[string]any{"type": "string", "description": "Unique step name; lowercase letters, digits, underscore or hyphen. The name runner is reserved for the job supervisor."}, "argv": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}, "description": "Executable and args, no shell expansion. {job_dir} and {step_dir} expand to owned output paths."}, "timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 86400}, "metrics_file": map[string]any{"type": "string", "description": "Optional relative JSONL progress path inside the owned step directory, e.g. output/metrics.jsonl; keeps nested trainer progress in job_inspect"}}}},
		"source_manifest":   map[string]any{"type": "object", "additionalProperties": false, "required": []string{"path", "sha256"}, "properties": map[string]any{"path": map[string]any{"type": "string"}, "sha256": map[string]any{"type": "string"}}, "description": "Optional hash-pinned relative wendy.sources.v1 file for up to 4096 source/checkpoint hashes and exact directory inventories. Validated before starting any job."},
		"source_hashes":     map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Optional relative source/checkpoint paths mapped to expected SHA-256"},
	}))}
	submit = append(submit, mutating()...)
	submit = append(submit,
		mcpgo.WithString("workflow_path", mcpgo.Description("Absolute local wendy.workflow.v1 file containing job, wait, selected artifacts and report fields. Supply this alone instead of other arguments.")),
		mcpgo.WithArray("artifact_files", mcpgo.WithStringItems(), mcpgo.Description("Optional selected relative reports to retrieve after a terminal observation; requires wait_seconds and artifact_destination. Same 64-file/16-MiB limits as job_artifacts_pull.")),
		mcpgo.WithArray("artifact_digests", mcpgo.WithStringItems(), mcpgo.Description("Optional subset of up to 16 collected files whose verified SHA-256 values should appear inline; at most 4096 encoded bytes. Avoids another manifest read when the task explicitly needs hashes.")),
		mcpgo.WithString("artifact_destination", mcpgo.Description("Absolute local report directory. Collection failures preserve the receipt; running jobs return artifacts_pending.")),
		mcpgo.WithObject("report_fields", mcpgo.Description("Optional retrieved JSON file paths mapped to RFC 6901 pointer arrays; up to eight reports/32 bounded scalar fields. Returns report hashes, values and explicit unavailable fields. A verified workflow receipt is saved with collected reports."), mcpgo.AdditionalProperties(map[string]any{"type": "array", "items": map[string]any{"type": "string"}})),
	)
	submit = append(submit, idempotent()...)
	submit = append(submit, openWorld()...)
	submitTool := mcpgo.NewTool("job_submit", submit...)
	submitSchema := submitTool.InputSchema.Properties["spec"].(map[string]any)
	submitSchema["required"] = []string{"name", "working_directory", "steps"}
	submitSchema["additionalProperties"] = false
	srv.AddTool(submitTool, s.handleJobSubmit)

	checkpoint := []mcpgo.ToolOption{
		mcpgo.WithDescription("Inspect a hash-pinned PyTorch checkpoint using an explicit remote Python runtime: bounded counters, saved RNG availability, source differences and matching environment adapter. Restricted CPU loading; never starts training or restores a job. The runtime executes with host-agent permissions."),
		mcpgo.WithString("checkpoint_path", mcpgo.Required(), mcpgo.Description("Absolute remote checkpoint path, at most 64 MiB")),
		mcpgo.WithString("sha256", mcpgo.Required(), mcpgo.Description("Expected checkpoint SHA-256 from its trusted receipt")),
		mcpgo.WithString("python", mcpgo.Required(), mcpgo.Description("Absolute remote Python interpreter with Torch/NumPy; no installation is performed")),
		mcpgo.WithString("source_directory", mcpgo.Description("Optional absolute remote project directory for source and adapter checks")),
	}
	// An explicit executable runs with host permissions; do not label this
	// capability automatically safe merely because the intended work is a read.
	checkpoint = append(checkpoint, mutating()...)
	checkpoint = append(checkpoint, openWorld()...)
	srv.AddTool(mcpgo.NewTool("checkpoint_inspect", checkpoint...), s.handleCheckpointInspect)

	compute := []mcpgo.ToolOption{mcpgo.WithDescription("Read current GPU identity, utilization, compute processes and CPU/memory/disk headroom in one bounded snapshot. Missing GPU tools stay unknown. Does not reserve the device, start training or change jobs. Requires authenticated Linux host access and python3.")}
	compute = append(compute, readOnly()...)
	compute = append(compute, openWorld()...)
	srv.AddTool(mcpgo.NewTool("compute_status", compute...), func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		conn, _, target := s.connectionSnapshot()
		if conn == nil {
			return errNotConnected(), nil
		}
		out, err := jobops.ComputeStatus(ctx, conn)
		if err != nil {
			return errResult(errCodeInternal, err.Error()), nil
		}
		out["target"] = target.Selector
		return jobJSON(out), nil
	})
	pull := []mcpgo.ToolOption{mcpgo.WithDescription("Pull selected host-job report snapshots to a local directory. Verifies hashes, skips unchanged files and returns a compact manifest reference; file bytes never enter tool output. Reads remote files and may replace selected local files. Maximum 64 files and 16 MiB transferred per call."), mcpgo.WithString("job_path", mcpgo.Required(), mcpgo.Description("Absolute remote job directory")), mcpgo.WithArray("files", mcpgo.Required(), mcpgo.WithStringItems(), mcpgo.Description("Selected relative files, for example status.json or control/metrics.jsonl")), mcpgo.WithString("destination", mcpgo.Required(), mcpgo.Description("Absolute local output directory"))}
	pull = append(pull, mutating()...)
	pull = append(pull, idempotent()...)
	pull = append(pull, openWorld()...)
	srv.AddTool(mcpgo.NewTool("job_artifacts_pull", pull...), s.handleJobArtifactsPull)
}

func (s *mcpServer) handleExperimentPlan(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := req.GetArguments()
	recipe, _ := args["recipe_path"].(string)
	destination, _ := args["destination"].(string)
	if len(args) != 2 {
		return errResult(errCodeInvalidArgument, "supply only recipe_path and destination"), nil
	}
	out, err := jobops.PlanExperiment(recipe, destination)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	return jobJSON(out), nil
}
func (s *mcpServer) handleJobInspect(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	jobPath, ok := req.GetArguments()["job_path"].(string)
	if !ok || jobPath == "" {
		return errResult(errCodeInvalidArgument, "job_path must be an absolute job directory"), nil
	}
	since := ""
	if v, exists := req.GetArguments()["since"]; exists {
		var valid bool
		since, valid = v.(string)
		if !valid {
			return errResult(errCodeInvalidArgument, "since must be a string"), nil
		}
	}
	wait, err := ros2Int(req, "wait_seconds", 0, 0, 30)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}

	if err = jobops.ValidateInspect(jobPath, jobops.InspectOptions{Since: since, WaitSeconds: wait}); err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	conn, _, target := s.connectionSnapshot()
	if conn == nil {
		return errNotConnected(), nil
	}
	result, err := jobops.Inspect(ctx, conn, jobPath, jobops.InspectOptions{Since: since, WaitSeconds: wait, Target: target.Selector})
	if err != nil {
		return errResult(errCodeInternal, err.Error()), nil
	}
	selector := target.Selector
	if selector == "" {
		selector = conn.Addr
	}
	result["target"] = selector
	return jobJSON(result), nil
}

func (s *mcpServer) handleJobArtifactsPull(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	jobPath, ok := req.GetArguments()["job_path"].(string)
	if !ok {
		return errResult(errCodeInvalidArgument, "job_path must be an absolute directory"), nil
	}
	destination, ok := req.GetArguments()["destination"].(string)
	if !ok || !filepath.IsAbs(destination) {
		return errResult(errCodeInvalidArgument, "destination must be an absolute local directory"), nil
	}
	raw, ok := req.GetArguments()["files"]
	if !ok {
		return errResult(errCodeInvalidArgument, "files is required"), nil
	}
	encoded, err := json.Marshal(raw)
	var files []string
	if err != nil || json.Unmarshal(encoded, &files) != nil || len(files) == 0 {
		return errResult(errCodeInvalidArgument, "files must be a nonempty array of relative paths"), nil
	}
	conn, _, target := s.connectionSnapshot()
	if conn == nil {
		return errNotConnected(), nil
	}
	result, err := jobops.Pull(ctx, conn, jobPath, destination, files)
	if err != nil {
		return errResult(errCodeInternal, err.Error()), nil
	}
	result["target"] = target.Selector
	return jobJSON(result), nil
}

// New workflow tools emit one JSON representation. Existing tools retain their
// compatibility text plus structuredContent; duplicating these new snapshots
// would needlessly repeat observations in clients that expose both to a model.
func jobJSON(value map[string]any) *mcpgo.CallToolResult {
	data, err := json.Marshal(value)
	if err != nil {
		return errResult(errCodeInternal, "cannot encode job result")
	}
	return okText(string(data))
}

func (s *mcpServer) handleJobSubmit(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if value, exists := req.GetArguments()["workflow_path"]; exists {
		name, ok := value.(string)
		if !ok || !filepath.IsAbs(name) || len(req.GetArguments()) != 1 {
			return errResult(errCodeInvalidArgument, "workflow_path must be an absolute local path and the only argument"), nil
		}
		spec, opts, err := jobops.LoadWorkflow(name)
		if err != nil {
			return errResult(errCodeInvalidArgument, err.Error()), nil
		}
		return s.submitWorkflow(ctx, spec, opts)
	}
	raw, inline := req.GetArguments()["spec"]
	file, fromFile := req.GetArguments()["spec_path"]
	if inline == fromFile {
		return errResult(errCodeInvalidArgument, "supply exactly one of spec or spec_path"), nil
	}
	wait, err := ros2Int(req, "wait_seconds", 0, 0, 3600)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	opts := jobops.SubmitOptions{WaitSeconds: wait}
	if value, exists := req.GetArguments()["artifact_files"]; exists {
		encoded, e := json.Marshal(value)
		if e != nil || json.Unmarshal(encoded, &opts.ArtifactFiles) != nil || len(opts.ArtifactFiles) == 0 {
			return errResult(errCodeInvalidArgument, "artifact_files must be a nonempty string array"), nil
		}
	}
	if value, exists := req.GetArguments()["artifact_digests"]; exists {
		encoded, e := json.Marshal(value)
		if e != nil || json.Unmarshal(encoded, &opts.ArtifactDigests) != nil || len(opts.ArtifactDigests) == 0 {
			return errResult(errCodeInvalidArgument, "artifact_digests must be a nonempty string array"), nil
		}
	}
	if value, exists := req.GetArguments()["artifact_destination"]; exists {
		var ok bool
		opts.ArtifactDestination, ok = value.(string)
		if !ok {
			return errResult(errCodeInvalidArgument, "artifact_destination must be a string"), nil
		}
	}
	if value, exists := req.GetArguments()["report_fields"]; exists {
		encoded, e := json.Marshal(value)
		if e != nil || json.Unmarshal(encoded, &opts.ReportFields) != nil || len(opts.ReportFields) == 0 {
			return errResult(errCodeInvalidArgument, "report_fields must map report paths to nonempty JSON pointer arrays"), nil
		}
	}
	if err = jobops.ValidateSubmitOptions(opts); err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	var spec jobops.Spec
	if fromFile {
		name, ok := file.(string)
		if !ok || !filepath.IsAbs(name) {
			return errResult(errCodeInvalidArgument, "spec_path must be an absolute local path"), nil
		}
		spec, err = jobops.LoadSpec(name)
	} else {
		var encoded []byte
		encoded, err = json.Marshal(raw)
		if err == nil {
			spec, err = jobops.ParseSpec(encoded)
		}
	}
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	return s.submitWorkflow(ctx, spec, opts)
}

func (s *mcpServer) submitWorkflow(ctx context.Context, spec jobops.Spec, opts jobops.SubmitOptions) (*mcpgo.CallToolResult, error) {
	conn, _, target := s.connectionSnapshot()
	if conn == nil {
		return errNotConnected(), nil
	}
	opts.Target = target.Selector
	if opts.Target == "" {
		opts.Target = conn.Addr
	}
	receipt, err := jobops.SubmitWorkflow(ctx, conn, spec, opts)
	if err != nil {
		return errResult(errCodeInternal, err.Error()), nil
	}
	receipt["target"] = opts.Target
	s.mu.RLock()
	compact := s.toolProfile == "workflow"
	v1 := s.toolProfile == "workflow-v1"
	s.mu.RUnlock()
	if v1 {
		receipt = compactWorkflowV1Result(receipt)
	} else if compact {
		receipt = compactWorkflowResult(receipt)
	}
	return jobJSON(receipt), nil
}

func (s *mcpServer) handleCheckpointInspect(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := req.GetArguments()
	path, _ := args["checkpoint_path"].(string)
	sha, _ := args["sha256"].(string)
	python, _ := args["python"].(string)
	source, _ := args["source_directory"].(string)
	if raw, present := args["source_directory"]; present {
		if _, ok := raw.(string); !ok {
			return errResult(errCodeInvalidArgument, "source_directory must be a string"), nil
		}
	}
	opts := jobops.CheckpointOptions{Path: path, SHA256: sha, Python: python, SourceDirectory: source}
	if err := jobops.ValidateCheckpoint(opts); err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	conn, _, target := s.connectionSnapshot()
	if conn == nil {
		return errNotConnected(), nil
	}
	result, err := jobops.InspectCheckpoint(ctx, conn, opts)
	if err != nil {
		return errResult(errCodeInternal, err.Error()), nil
	}
	result["target"] = target.Selector
	return jobJSON(result), nil
}
