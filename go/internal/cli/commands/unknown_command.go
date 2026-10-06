package commands

import (
	"errors"
	"fmt"
	"strings"
)

// UnknownSubcommandError returns a non-nil error when args name a subcommand
// that does not exist under a group command, or a bundled --device argument
// would make Cobra misidentify the command.
//
// Cobra does not do this for us. Its legacyArgs check rejects an unknown
// subcommand only for the root command, because it tests !cmd.HasParent()
// before erroring. Any nested group accepts the stray token as a positional
// argument instead. The group has no action of its own, so Command.execute
// takes its !c.Runnable() branch and returns flag.ErrHelp, which ExecuteC
// renders as that group's help page and a zero exit code. The result is that
// `wendy banana` fails loudly while `wendy device banana` prints the ordinary
// `wendy device` help and reports success, which is indistinguishable from
// having run `wendy device` on purpose.
//
// This runs ahead of cobra rather than through Command.Args because execute
// honours the help flag before it validates arguments: `wendy device banana
// --help` never reaches a validator, and that is the spelling people actually
// type when they are exploring an unfamiliar command.
func UnknownSubcommandError(args []string) error {
	if err := bundledDeviceArgumentError(args); err != nil {
		return err
	}
	// Resolution and flag parsing both mutate command state, so probe a
	// throwaway tree and leave the caller's root untouched.
	target, remaining, err := NewRootCmd().Find(args)
	if err != nil || target == nil {
		// Cobra found its own problem here; let it report that itself.
		return nil
	}
	// Only groups are affected. A command with an action of its own is entitled
	// to positional arguments, and validates them itself.
	if !target.HasSubCommands() || target.Runnable() {
		return nil
	}

	// Let pflag split flags from positionals so that a flag value is never
	// mistaken for a subcommand, as in `wendy device --device foo bar`.
	target.InitDefaultHelpFlag()
	if err := target.Flags().Parse(remaining); err != nil {
		// A malformed flag is cobra's error to report, not ours.
		return nil
	}
	positional := target.Flags().Args()
	if len(positional) == 0 {
		return nil
	}

	// The message keeps cobra's own layout for its root-level error; JSON mode
	// reports the suggestions and the help pointer as next steps.
	headline := fmt.Sprintf("unknown command %q for %q", positional[0], target.CommandPath())
	msg := headline
	var steps []string
	if suggestions := target.SuggestionsFor(positional[0]); len(suggestions) > 0 {
		msg += "\n\nDid you mean this?\n"
		for _, s := range suggestions {
			msg += fmt.Sprintf("\t%s\n", s)
			steps = append(steps, fmt.Sprintf("Did you mean '%s %s'?", target.CommandPath(), s))
		}
	}
	help := fmt.Sprintf("Run '%s --help' to see the available commands.", target.CommandPath())
	msg += "\n" + help
	steps = append(steps, help)
	return markUsage(withNextStepsText(errors.New(headline), msg, steps...))
}

// Inspect only the known global-option prefix. After a command or --, values
// and forwarded programs may legitimately contain text such as "--device x".
func bundledDeviceArgumentError(args []string) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json", strings.HasPrefix(arg, "--json="), strings.HasPrefix(arg, "--device="):
			continue
		case arg == "--device":
			i++ // A separate device value can itself contain spaces.
			continue
		case strings.HasPrefix(arg, "--device "), strings.HasPrefix(arg, "--device\t"), strings.HasPrefix(arg, "--device\n"):
			return markUsage(errors.New("invalid --device argument: the option and its value arrived as one argument\n" +
				"Store only the device selector in DEVICE and pass it separately, for example:\n" +
				"  wendy --device \"$DEVICE\" device ros2 topics\n" +
				"In zsh, use an argument array if you need to store both the option and value. " +
				"This is a command syntax error; retrying the unchanged command will not help."))
		default:
			return nil
		}
	}
	return nil
}
