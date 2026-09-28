package commands

import "strings"

// nextStepsError attaches recovery steps to an error. Error() keeps the
// long-standing layout, the cause followed by each step on its own line
// indented two spaces, so every consumer of the message text is unchanged.
// JSON mode reads the steps through NextSteps and reports them separately.
type nextStepsError struct {
	err   error
	steps []string
}

// withNextSteps attaches steps to err. nil stays nil; no steps returns err.
func withNextSteps(err error, steps ...string) error {
	if err == nil || len(steps) == 0 {
		return err
	}
	return &nextStepsError{err: err, steps: steps}
}

func (e *nextStepsError) Error() string {
	return e.err.Error() + "\n  " + strings.Join(e.steps, "\n  ")
}

func (e *nextStepsError) Unwrap() error { return e.err }

// NextSteps returns a copy of the steps attached to this error.
func (e *nextStepsError) NextSteps() []string { return append([]string(nil), e.steps...) }

// NextSteps collects the recovery steps attached anywhere in err's chain,
// outermost first, following both branches of errors.Join. Any error type
// with a NextSteps() []string method contributes.
func NextSteps(err error) []string {
	var steps []string
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if carrier, ok := e.(interface{ NextSteps() []string }); ok {
			steps = append(steps, carrier.NextSteps()...)
		}
		switch wrapped := e.(type) {
		case interface{ Unwrap() []error }:
			for _, inner := range wrapped.Unwrap() {
				walk(inner)
			}
		case interface{ Unwrap() error }:
			walk(wrapped.Unwrap())
		}
	}
	walk(err)
	return steps
}
