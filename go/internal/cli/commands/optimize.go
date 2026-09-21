package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/go/internal/cli/optimize"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

type optimizeOptions struct {
	Dir     string
	Arch    string
	Fix     bool
	Agentic bool
}

// loadOptConfig loads wendy.json from dir, returning (nil, "") when absent.
func loadOptConfig(dir string) (*appconfig.AppConfig, string) {
	p := filepath.Join(dir, "wendy.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, ""
	}
	cfg, err := appconfig.LoadFromBytes(data)
	if err != nil {
		return nil, string(data)
	}
	return cfg, string(data)
}

// runOptimize is the testable core: no printing, no os.Exit.
func runOptimize(opts optimizeOptions) (optimize.Report, []optimize.AppliedFix, error) {
	arch := opts.Arch
	if arch == "" {
		arch = "arm64"
	}
	cfg, _ := loadOptConfig(opts.Dir)

	targets, err := optimize.DiscoverTargets(opts.Dir, cfg, arch)
	if err != nil {
		return optimize.Report{}, nil, err
	}
	findings := optimize.Analyze(targets, optimize.DefaultAnalyzers())

	var applied []optimize.AppliedFix
	if opts.Fix {
		applied, err = optimize.ApplyFixes(findings)
		if err != nil {
			return optimize.Report{}, applied, err
		}
		// Recompute post-fix so callers see the residual state.
		targets, err = optimize.DiscoverTargets(opts.Dir, cfg, arch)
		if err != nil {
			return optimize.Report{}, applied, err
		}
		findings = optimize.Analyze(targets, optimize.DefaultAnalyzers())
	}

	return optimize.BuildReport(targets, findings), applied, nil
}

func newOptimizeCmd() *cobra.Command {
	var (
		archFlag     string
		fixFlag      bool
		agenticFlag  bool
		severityFlag string
	)

	cmd := &cobra.Command{
		Use:    "optimize",
		Short:  "Analyze the project's build config for missed optimizations",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if agenticFlag && fixFlag {
				fmt.Fprintln(os.Stderr, "error: --agentic and --fix cannot be combined; --agentic only emits an analysis bundle")
				os.Exit(2)
			}
			cwd, err := os.Getwd()
			if err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				os.Exit(2)
			}
			threshold, err := optimize.ParseSeverity(severityFlag)
			if err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				os.Exit(2)
			}
			opts := optimizeOptions{Dir: cwd, Arch: archFlag, Fix: fixFlag, Agentic: agenticFlag}

			if agenticFlag {
				arch := archFlag
				if arch == "" {
					arch = "arm64"
				}
				cfg, raw := loadOptConfig(cwd)
				targets, derr := optimize.DiscoverTargets(cwd, cfg, arch)
				if derr != nil {
					fmt.Fprintln(os.Stderr, derr.Error())
					os.Exit(2)
				}
				findings := optimize.Analyze(targets, optimize.DefaultAnalyzers())
				bundle := optimize.BuildBundle(cwd, raw, targets, findings)
				data, merr := json.MarshalIndent(bundle, "", "  ")
				if merr != nil {
					return merr
				}
				// The bundle embeds verbatim file contents (Dockerfiles,
				// requirements.txt, wendy.json) so an agent has full context.
				// Warn on stderr — these may carry secrets (ARG/ENV tokens,
				// private registry URLs) and the JSON is meant to be handed to
				// an external agent. stderr keeps the warning out of the piped
				// JSON on stdout.
				cliNotice("note: this --agentic bundle contains the verbatim contents of your Dockerfile(s), requirements.txt, and wendy.json. Review it for secrets (ARG/ENV tokens, private registry URLs) before sending it to an external agent.")
				// Bundle JSON goes to stdout so it can be piped to an agent;
				// cmd.Println would route to stderr (cobra's OutOrStderr).
				fmt.Println(string(data))
				return nil
			}

			rep, applied, rerr := runOptimize(opts)
			if rerr != nil {
				fmt.Fprintln(os.Stderr, rerr.Error())
				os.Exit(2)
			}

			if fixFlag {
				cliSuccess("Applied fixes:")
				for _, a := range applied {
					if a.Applied {
						fmt.Printf("  %s — %s\n", a.Fix.File, a.Fix.Description)
					} else {
						fmt.Printf("  %s — skipped (%s)\n", a.Fix.File, a.Reason)
					}
				}
			}

			// Emit on stdout (fmt, not cmd.Println) so --json/report output can
			// be captured and piped, consistent with the rest of the CLI.
			if jsonOutput {
				data, merr := json.MarshalIndent(rep, "", "  ")
				if merr != nil {
					return merr
				}
				fmt.Println(string(data))
			} else {
				fmt.Print(optimize.RenderHuman(rep))
			}

			if rep.MaxSeverity() >= threshold && len(rep.Findings) > 0 {
				os.Exit(1)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&archFlag, "arch", "", "Target architecture (default arm64)")
	cmd.Flags().BoolVar(&fixFlag, "fix", false, "Apply safe, deterministic fixes")
	cmd.Flags().BoolVar(&agenticFlag, "agentic", false, "Emit an agent context bundle instead of a report")
	cmd.Flags().StringVar(&severityFlag, "severity", "warning", "Minimum severity that triggers a non-zero exit (info|warning|error)")
	return cmd
}
