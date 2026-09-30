package main

import (
	"errors"
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// internalErrorStep is the recovery step for a bug in wendy itself.
const internalErrorStep = "This is a bug in wendy. Please report it with the command you ran and the stack trace printed with this error."

// internalError is a panic recovered from a command. It is reported like any
// other failure, with its own class and exit status, instead of Go's crash
// output (which exits 2, the usage-error status, and is not an envelope).
type internalError struct {
	value any
	stack []byte
}

func (e *internalError) Error() string {
	return fmt.Sprintf("internal error: %v\n  %s", e.value, internalErrorStep)
}

// NextSteps exposes the recovery step to JSON mode's next_steps.
func (e *internalError) NextSteps() []string { return []string{internalErrorStep} }

// Unwrap exposes a panic value that is itself an error.
func (e *internalError) Unwrap() error {
	if err, ok := e.value.(error); ok {
		return err
	}
	return nil
}

// executeRecovering runs execute, turning a panic in it into an
// *internalError that carries the panicking goroutine's stack.
func executeRecovering(execute func() (*cobra.Command, error)) (executed *cobra.Command, err error) {
	defer func() {
		if value := recover(); value != nil {
			executed, err = nil, &internalError{value: value, stack: debug.Stack()}
		}
	}()
	return execute()
}

// commandFor returns the command a failed invocation ran. A recovered panic
// leaves executed nil, so the command the arguments name is looked up
// instead: the failure is still tracked, and a hidden "__" helper still
// reports as text to the parent wendy reading its stderr.
func commandFor(root *cobra.Command, args []string, executed *cobra.Command, err error) *cobra.Command {
	var internal *internalError
	if executed != nil || !errors.As(err, &internal) {
		return executed
	}
	if found, _, findErr := root.Find(args); findErr == nil {
		return found
	}
	return nil
}
