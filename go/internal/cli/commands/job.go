package commands

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/jobops"
)

func newJobCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "job", Short: "Submit bounded host jobs, inspect progress and retrieve verified reports", GroupID: "manage"}
	var since string
	var wait int
	inspect := &cobra.Command{Use: "inspect <absolute-job-directory>", Short: "Return bounded progress, process identity, freshness and an evidence reference", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := jobops.ValidateInspect(args[0], jobops.InspectOptions{Since: since, WaitSeconds: wait}); err != nil {
			return err
		}
		conn, err := connectToAgent(cmd.Context(), monitoringOptions(true)...)
		if err != nil {
			return err
		}
		defer conn.Close()
		result, err := jobops.Inspect(cmd.Context(), conn, args[0], jobops.InspectOptions{Since: since, WaitSeconds: wait, Target: deviceFlag})
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	inspect.Flags().StringVar(&since, "since", "", "Return a compact unchanged acknowledgement for this cursor")
	inspect.Flags().IntVar(&wait, "wait", 0, "Wait up to 30 seconds for a change; requires --since")
	cmd.AddCommand(inspect, newJobArtifactsCmd(), newJobSubmitCmd(), newCheckpointCmd(), newExperimentPlanCmd())
	return cmd
}

func newExperimentPlanCmd() *cobra.Command {
	var destination string
	cmd := &cobra.Command{Use: "plan <recipe.json>", Short: "Expand a local parameter recipe into a new workflow without executing it", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		result, err := jobops.PlanExperiment(args[0], destination)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVarP(&destination, "output", "o", "", "Absolute new local workflow path; never overwrites")
	cmd.MarkFlagRequired("output")
	return cmd
}

func newJobArtifactsCmd() *cobra.Command {
	parent := &cobra.Command{Use: "artifacts", Short: "Retrieve selected job evidence with verification"}
	var files []string
	var output string
	pull := &cobra.Command{Use: "pull <absolute-job-directory>", Short: "Pull selected snapshots, skip unchanged hashes and return a manifest", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := connectToAgent(cmd.Context(), monitoringOptions(true)...)
		if err != nil {
			return err
		}
		defer conn.Close()
		result, err := jobops.Pull(cmd.Context(), conn, args[0], output, files)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	pull.Flags().StringArrayVar(&files, "file", nil, "Relative file to retrieve; repeat for several files (maximum 64)")
	pull.Flags().StringVarP(&output, "output", "o", "", "Local destination directory")
	pull.MarkFlagRequired("file")
	pull.MarkFlagRequired("output")
	parent.AddCommand(pull)
	return parent
}

func newComputeStatusCmd() *cobra.Command {
	compute := &cobra.Command{Use: "compute-status", Short: "Read GPU activity and host capacity without changing workloads", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		conn, err := connectToAgent(cmd.Context(), monitoringOptions(true)...)
		if err != nil {
			return err
		}
		defer conn.Close()
		out, err := jobops.ComputeStatus(cmd.Context(), conn)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}}

	return compute
}

func newJobSubmitCmd() *cobra.Command {
	var wait int
	var files []string
	var output string
	var fields []string
	var digests []string
	var workflow bool
	cmd := &cobra.Command{Use: "submit <spec.json>", Short: "Start a named durable job; an identical retry returns the original receipt", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if wait < 0 || wait > 3600 {
			return fmt.Errorf("wait must be 0..3600")
		}
		opts := jobops.SubmitOptions{WaitSeconds: wait, ArtifactFiles: files, ArtifactDigests: digests, ArtifactDestination: output}
		if len(fields) > 0 {
			opts.ReportFields = map[string][]string{}
		}
		for _, field := range fields {
			name, pointer, ok := strings.Cut(field, "#")
			if !ok {
				return fmt.Errorf("report-field must be relative.json#/pointer")
			}
			opts.ReportFields[name] = append(opts.ReportFields[name], pointer)
		}
		if err := jobops.ValidateSubmitOptions(opts); err != nil {
			return err
		}
		var spec jobops.Spec
		var err error
		if workflow {
			for _, flag := range []string{"wait", "artifact", "output", "report-field", "artifact-digest"} {
				if cmd.Flags().Changed(flag) {
					return fmt.Errorf("workflow file owns %s; do not also pass that flag", flag)
				}
			}
			spec, opts, err = jobops.LoadWorkflow(args[0])
		} else {
			spec, err = jobops.LoadSpec(args[0])
		}
		if err != nil {
			return err
		}
		conn, err := connectToAgent(cmd.Context(), monitoringOptions(true)...)
		if err != nil {
			return err
		}
		defer conn.Close()
		opts.Target = deviceFlag
		receipt, err := jobops.SubmitWorkflow(cmd.Context(), conn, spec, opts)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(receipt)
	}}
	cmd.Flags().IntVar(&wait, "wait", 0, "Wait up to 3600 seconds for terminal evidence through bounded host RPCs; expiry never cancels the job")
	cmd.Flags().StringArrayVar(&files, "artifact", nil, "Selected relative report to retrieve after terminal evidence; repeat, requires --wait and --output")
	cmd.Flags().StringVarP(&output, "output", "o", "", "Absolute local artifact directory; collection errors retain the confirmed job receipt")
	cmd.Flags().StringArrayVar(&digests, "artifact-digest", nil, "Collected file whose verified SHA-256 should appear inline; repeat, at most 16 files/4096 bytes")
	cmd.Flags().StringArrayVar(&fields, "report-field", nil, "Verified scalar field as relative.json#/pointer; repeat, requires --artifact, --wait and --output")
	cmd.Flags().BoolVar(&workflow, "workflow", false, "Read a saved wendy.workflow.v1 execution/evidence contract instead of a plain job specification")
	return cmd
}

func newCheckpointCmd() *cobra.Command {
	parent := &cobra.Command{Use: "checkpoint", Short: "Inspect checkpoint metadata and source compatibility without starting training"}
	var opts jobops.CheckpointOptions
	cmd := &cobra.Command{Use: "inspect <absolute-remote-checkpoint>", Short: "Read bounded, restricted checkpoint metadata with an explicit remote Python runtime", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		opts.Path = args[0]
		if err := jobops.ValidateCheckpoint(opts); err != nil {
			return err
		}
		conn, err := connectToAgent(cmd.Context(), monitoringOptions(true)...)
		if err != nil {
			return err
		}
		defer conn.Close()
		result, err := jobops.InspectCheckpoint(cmd.Context(), conn, opts)
		if err != nil {
			return err
		}
		result["target"] = deviceFlag
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVar(&opts.Python, "python", "", "Absolute remote interpreter with Torch/NumPy; executes the fixed inspector with host-agent permissions")
	cmd.Flags().StringVar(&opts.SHA256, "sha256", "", "Expected SHA-256 from the trusted checkpoint receipt")
	cmd.Flags().StringVar(&opts.SourceDirectory, "source-directory", "", "Absolute remote project directory for source and environment-adapter checks")
	cmd.MarkFlagRequired("python")
	cmd.MarkFlagRequired("sha256")
	parent.AddCommand(cmd)
	return parent
}
