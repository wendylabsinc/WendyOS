package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
)

// errorEnvelope is the single line JSON mode writes to stderr for a failed
// command. Its shape is a published contract (see "Errors in JSON mode" in
// docs/clients/wendy-cli/global-flags.md): add fields, never rename or
// remove them.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string   `json:"code"`
	Exit      int      `json:"exit"`
	Message   string   `json:"message"`
	Retryable bool     `json:"retryable"`
	NextSteps []string `json:"next_steps"`
}

// reportFailure reports err the way the invocation asked for (a JSON envelope
// or styled text, both on stderr) and returns the process exit status.
// Cancellations the user chose print nothing and exit 0, as before.
func reportFailure(stderr io.Writer, err error, executed *cobra.Command, args []string) int {
	if err == nil || errors.Is(err, commands.ErrUserCancelled) || errors.Is(err, commands.ErrDefaultCleared) {
		return 0
	}
	class := errorClass(err)
	outcome := outcomeForClass(class)
	// A panic's stack is what a bug report needs. JSON mode prints it before
	// the envelope, which stays the last line; text mode after the message.
	var internal *internalError
	isInternal := errors.As(err, &internal)
	if commands.JSONErrorsRequested(executed, args) {
		if isInternal {
			fmt.Fprintf(stderr, "%s\n", bytes.TrimRight(internal.stack, "\n"))
		}
		if writeErrorEnvelope(stderr, newErrorEnvelope(err, class, outcome, executed)) == nil {
			return outcome.exit
		}
		// Encoding strings cannot realistically fail; if it does, fall
		// through so the failure is still reported, as text.
	}
	fmt.Fprintln(stderr, renderError(err))
	if isInternal {
		fmt.Fprintf(stderr, "\n%s", internal.stack)
	}
	return outcome.exit
}

func newErrorEnvelope(err error, class string, outcome errorOutcome, executed *cobra.Command) errorEnvelope {
	code := class
	if code == "" || code == "other" {
		code = "error"
	}
	message, steps := plainErrorReport(err)
	if class == "cli_usage" && len(steps) == 0 && executed != nil {
		steps = append(steps, fmt.Sprintf("Run '%s --help' for usage.", executed.CommandPath()))
	}
	return errorEnvelope{Error: errorBody{
		Code:      code,
		Exit:      outcome.exit,
		Message:   message,
		Retryable: outcome.retryable,
		NextSteps: steps,
	}}
}

// plainErrorReport returns err as the unstyled text renderError would show,
// minus the recovery steps, which it returns separately: the steps
// formatError adds and those attached anywhere in err's chain.
func plainErrorReport(err error) (string, []string) {
	var text string
	var steps []string
	var diagnostic interface {
		error
		CLIMessage() string
	}
	if errors.As(err, &diagnostic) {
		// CLIMessage is a styled rendering of the text Error() already holds.
		text = err.Error()
	} else {
		f := formatErrorParts(err)
		text, steps = f.text, f.steps
	}
	steps = append(steps, commands.NextSteps(err)...)
	text = commands.StripNextSteps(text, err)
	for _, step := range steps {
		text = withoutStep(text, step)
	}
	return strings.TrimSpace(ansi.Strip(text)), cleanSteps(steps)
}

// withoutStep removes the first occurrence of step from text, together with
// what separates it from the text before it: the standard indented line
// ("\n  step"), a plain line ("\nstep"), or a sentence in the same line
// (" step"), tried in that order.
func withoutStep(text, step string) string {
	for _, sep := range []string{"\n  ", "\n", " "} {
		if strings.Contains(text, sep+step) {
			return strings.Replace(text, sep+step, "", 1)
		}
	}
	return text
}

// cleanSteps strips styling, drops blank and repeated steps, and never
// returns nil, so next_steps always encodes as a JSON array.
func cleanSteps(steps []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, step := range steps {
		step = strings.TrimSpace(ansi.Strip(step))
		if step == "" || seen[step] {
			continue
		}
		seen[step] = true
		out = append(out, step)
	}
	return out
}

// writeErrorEnvelope writes env as one line. HTML escaping is off so advice
// such as "ssh wendy@<host>" keeps its angle brackets as written.
func writeErrorEnvelope(w io.Writer, env errorEnvelope) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(env); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}
