package commands

import "strings"

// nextStepsError attaches recovery steps to an error. Error() keeps the
// long-standing layout, the cause followed by each step on its own line
// indented two spaces, so every consumer of the message text is unchanged.
// JSON mode reads the steps through NextSteps and reports them separately.
type nextStepsError struct {
	err   error
	steps []string
	// text, when set, is the full message for an error that lays its steps
	// out in a way of its own; err's message is then the text without them.
	text string
}

// withNextSteps attaches steps to err. nil stays nil; no steps returns err.
func withNextSteps(err error, steps ...string) error {
	if err == nil || len(steps) == 0 {
		return err
	}
	return &nextStepsError{err: err, steps: steps}
}

// withNextStepsText is withNextSteps for a message whose steps are already
// laid out in a form of its own: text is the full message a person sees,
// and err the same message without the steps.
func withNextStepsText(err error, text string, steps ...string) error {
	if err == nil || len(steps) == 0 {
		return err
	}
	return &nextStepsError{err: err, steps: steps, text: text}
}

func (e *nextStepsError) Error() string {
	if e.text != "" {
		return e.text
	}
	return e.err.Error() + "\n  " + strings.Join(e.steps, "\n  ")
}

func (e *nextStepsError) Unwrap() error { return e.err }

// NextSteps returns a copy of the steps attached to this error.
func (e *nextStepsError) NextSteps() []string { return append([]string(nil), e.steps...) }

// MessageWithoutSteps is the message with the steps left out.
func (e *nextStepsError) MessageWithoutSteps() string { return e.err.Error() }

// NextSteps collects the recovery steps attached anywhere in err's chain,
// outermost first, following both branches of errors.Join. Any error type
// with a NextSteps() []string method contributes.
func NextSteps(err error) []string {
	var steps []string
	walkErrorChain(err, func(e error) {
		if carrier, ok := e.(interface{ NextSteps() []string }); ok {
			steps = append(steps, carrier.NextSteps()...)
		}
	})
	return steps
}

// StripNextSteps removes, from text that shows err, the steps of the errors
// in err's chain that lay their steps out in a form of their own (those with
// a MessageWithoutSteps method): each one's full message is replaced by its
// message without the steps. Steps in the standard layout, a line
// "\n  step", are left for the caller to cut, since text may have been
// reworded around them.
func StripNextSteps(text string, err error) string {
	walkErrorChain(err, func(e error) {
		if custom, ok := e.(interface{ MessageWithoutSteps() string }); ok {
			text = strings.Replace(text, e.Error(), custom.MessageWithoutSteps(), 1)
		}
	})
	return text
}

// walkErrorChain calls visit on err and every error it wraps, outermost
// first, following both branches of errors.Join.
func walkErrorChain(err error, visit func(error)) {
	if err == nil {
		return
	}
	visit(err)
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, inner := range wrapped.Unwrap() {
			walkErrorChain(inner, visit)
		}
	case interface{ Unwrap() error }:
		walkErrorChain(wrapped.Unwrap(), visit)
	}
}
