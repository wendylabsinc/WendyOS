package commands

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// usageError marks a command-line syntax error: the invocation itself is
// wrong, so running it again unchanged cannot succeed. The message is the
// wrapped error's, unchanged.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// markUsage tags err as a usage error; nil stays nil.
func markUsage(err error) error {
	if err == nil {
		return nil
	}
	return &usageError{err: err}
}

// usageErrorf builds a usage error from a format string.
func usageErrorf(format string, args ...any) error {
	return markUsage(fmt.Errorf(format, args...))
}

// markUsageErrors tags the usage errors cobra reports through hooks that can
// be wrapped: every positional-argument validator in the tree, and flag
// parsing, through the root's flag-error function, which every subcommand
// inherits. Call it after the tree is complete and Args validators are set.
func markUsageErrors(root *cobra.Command) {
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return markUsage(err) })
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if validate := cmd.Args; validate != nil {
			cmd.Args = func(c *cobra.Command, args []string) error {
				return markUsage(validate(c, args))
			}
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
}

// cobraUsageErrors match, exactly, the usage errors cobra builds with
// fmt.Errorf outside any hook markUsageErrors can wrap: an unknown
// subcommand of the root (legacyArgs, from Command.Find, with cobra's
// optional suggestions), a missing required flag, and the three flag-group
// violations (checked in Command.execute after PreRunE). They are anchored
// at both ends so that a message a device or agent sent back, which commands
// such as ros2 return unwrapped, cannot pass for one.
// TestIsUsageErrorRecognisesEveryCobraSource pins them against the vendored
// cobra, so an upgrade that rewords one fails loudly.
var cobraUsageErrors = []*regexp.Regexp{
	regexp.MustCompile(`^unknown command "(?:[^"\\]|\\.)*" for "wendy[^"]*"(?:\n\nDid you mean this\?\n(?:\t[^\n]*\n)+)?$`),
	regexp.MustCompile(`^required flag\(s\) "[^\n]*" not set$`),
	regexp.MustCompile(`^if any flags in the group \[[^\]\n]*\] are set they must all be set; missing \[[^\]\n]*\]$`),
	regexp.MustCompile(`^at least one of the flags in the group \[[^\]\n]*\] is required$`),
	regexp.MustCompile(`^if any flags in the group \[[^\]\n]*\] are set none of the others can be; \[[^\]\n]*\] were all set$`),
}

// IsUsageError reports whether err is a command-line syntax error.
func IsUsageError(err error) bool {
	if err == nil {
		return false
	}
	var usage *usageError
	var unknownFlag *pflag.NotExistError
	var missingValue *pflag.ValueRequiredError
	var invalidValue *pflag.InvalidValueError
	var invalidSyntax *pflag.InvalidSyntaxError
	if errors.As(err, &usage) || errors.As(err, &unknownFlag) || errors.As(err, &missingValue) ||
		errors.As(err, &invalidValue) || errors.As(err, &invalidSyntax) {
		return true
	}
	// Cobra returns these unwrapped. Anything wrapped in context came from a
	// command, not from cobra's own validation.
	if errors.Unwrap(err) != nil {
		return false
	}
	msg := err.Error()
	for _, shape := range cobraUsageErrors {
		if shape.MatchString(msg) {
			return true
		}
	}
	return false
}
