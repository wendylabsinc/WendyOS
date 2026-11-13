package commands

import (
	"context"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

type invocationAnalyticsKey struct{}
type invocationAnalyticsState struct {
	firstRun bool
	excluded bool
}

// PrepareInvocationAnalytics records a launch before Cobra can return for help,
// invalid flags, or argument validation. Only command-tree names enter telemetry;
// an unknown command is attributed to its last recognized parent.
//
// Keep this separate from the normal pre-run: resolving a command must not start
// device discovery, migrate credentials, or run update checks. Explicit local
// diagnostics and privileged helpers retain their no-telemetry contract.
func PrepareInvocationAnalytics(root *cobra.Command, args []string) *cobra.Command {
	root.InitDefaultHelpCmd()
	// Cobra adds this hidden command only inside ExecuteC. Resolve its bounded
	// names now as well, without keeping a stub that could intercept execution.
	completion := &cobra.Command{
		Use: "__complete", Aliases: []string{"__completeNoDesc"},
		Hidden: true, DisableFlagParsing: true,
	}
	root.AddCommand(completion)
	target, remaining, _ := root.Find(args)
	if target == nil {
		target = root
	}
	commandPath := target.CommandPath()
	root.RemoveCommand(completion)
	if target == completion {
		target = root // ExecuteC will return Cobra's real completion command.
	}
	ctx := root.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	state := &invocationAnalyticsState{excluded: excludeInvocationAnalytics(target, remaining)}
	ctx = context.WithValue(ctx, invocationAnalyticsKey{}, state)
	root.SetContext(ctx)
	target.SetContext(ctx)
	if !state.excluded {
		// A malformed/unreadable config may contain an opt-out. Never replace it
		// with defaults just to send an event about the failure.
		if cfg, err := config.Load(); err == nil {
			notice := !strings.Contains(target.CommandPath(), " install jobs")
			state.firstRun = startCommandAnalytics(target, cfg, notice, commandPath)
		}
	}
	return target
}

// InvocationAnalyticsExcluded preserves the startup decision even when Cobra
// rejects a flag before parsing the read-only option that follows it.
func InvocationAnalyticsExcluded(cmd *cobra.Command) bool {
	if cmd != nil && cmd.Root().Context() != nil {
		if state, ok := cmd.Root().Context().Value(invocationAnalyticsKey{}).(*invocationAnalyticsState); ok {
			return state.excluded
		}
	}
	return false
}

func excludeInvocationAnalytics(cmd *cobra.Command, args []string) bool {
	switch cmd.Name() {
	case "__ble-check", "__session-broker", "__usb-setup", "__t234-write", "__bmap-write", "__gpt-relocate", "__usb-driver", "open-browser":
		return true
	}
	switch cmd.CommandPath() {
	case "wendy doctor", "wendy install plan", "wendy os install plan", "wendy analytics disable":
		return true
	case "wendy update":
		if runsAsForeignUser() {
			return true
		}
		// Inspect only this boolean flag, without parsing/mutating the real
		// command before Cobra does. Values after -- are positional arguments.
		check := false
		for i := 0; i < len(args); i++ {
			arg := args[i]
			if arg == "--" {
				break
			}
			if arg == "--check" {
				check = true
			} else if value, ok := strings.CutPrefix(arg, "--check="); ok {
				v, err := strconv.ParseBool(value)
				// Invalid values still express a read-only check. Let Cobra
				// report the syntax error without enabling telemetry.
				if err != nil {
					return true
				}
				check = v
			} else if strings.HasPrefix(arg, "--") && !strings.Contains(arg, "=") {
				if flag := cmd.Flags().Lookup(strings.TrimPrefix(arg, "--")); flag != nil && flag.NoOptDefVal == "" {
					i++ // A value named --check is not the updater's boolean flag.
				}
			} else if len(arg) == 2 && arg[0] == '-' {
				if flag := cmd.Flags().ShorthandLookup(arg[1:]); flag != nil && flag.NoOptDefVal == "" {
					i++
				}
			}
		}
		return check
	}
	return false
}

// Direct command users/tests still initialize in pre-run. The executable's
// earlier preparation supplies the original first-run result without sending a
// duplicate launch or displaying the notice twice.
func initializeCommandAnalytics(cmd *cobra.Command, cfg *config.Config, notice bool) bool {
	if ctx := cmd.Root().Context(); ctx != nil {
		if state, ok := ctx.Value(invocationAnalyticsKey{}).(*invocationAnalyticsState); ok {
			return state.firstRun
		}
	}
	return startCommandAnalytics(cmd, cfg, notice, cmd.CommandPath())
}

func startCommandAnalytics(cmd *cobra.Command, cfg *config.Config, notice bool, commandPath string) bool {
	firstRun := analytics.Init(cfg)
	if firstRun && notice {
		showFirstRunNotice(cmd, cfg)
	}
	analytics.TrackStage(cmd.Context(), "cli_seen", commandPath, "completed", "", nil)
	return firstRun
}
