// Package robotwizard runs the calibration procedure loop: check
// preconditions, tell the operator what will physically happen, collect a
// sample, repeat, solve, judge against a budget, install or refuse.
//
// That loop is the same on every robot. Only its contents differ, and those
// come from the profile — which joints, what "limp" is called on this machine,
// what counts as good enough. Nothing here knows what a humanoid is or what an
// arm is, and adding a robot must never mean editing this package.
package robotwizard

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// StepAction is what an operator did when asked to perform a step.
type StepAction int

const (
	// StepDone: they did it.
	StepDone StepAction = iota
	// StepSkip: they chose not to, and that must be recorded as not measured
	// rather than as a zero. "Did not touch" and "cannot tell" are different
	// answers and have to stay different.
	StepSkip
	// StepAbort: they stopped. What is already measured stays checkpointed.
	StepAbort
)

// Prompter is every way the wizard talks to a person. An interface because a
// procedure that can only be exercised by a human at a real terminal is a
// procedure with no tests.
type Prompter interface {
	// Announce states what will physically happen, before it happens. The
	// wizard calls it before asking for anything, never after.
	Announce(heading string, lines ...string)
	// Confirm asks a yes/no question with no default. A default of yes on "the
	// robot will go limp" is not a question.
	Confirm(question string) (bool, error)
	// Step asks the operator to do something and waits. It returns what they
	// chose to do.
	Step(prompt string) (StepAction, error)
	// Choose offers a list and returns the index chosen.
	Choose(title string, options []string) (int, error)
	// Infof reports progress between steps.
	Infof(format string, args ...any)
}

// ErrAborted is returned when the operator stopped the procedure. What was
// already measured is checkpointed, so the next run resumes rather than
// restarts.
var ErrAborted = errors.New("calibration stopped by the operator")

// TerminalPrompter drives the wizard from a real terminal.
//
// Deliberately plain line-reading rather than the TUI picker the rest of the
// CLI uses: a calibration step is "go and move the robot, then come back", so
// the terminal spends most of its time being read by someone standing next to a
// machine, not being interacted with.
type TerminalPrompter struct {
	In  io.Reader
	Out io.Writer

	reader *bufio.Reader
}

// NewTerminalPrompter returns a prompter reading from in and writing to out.
func NewTerminalPrompter(in io.Reader, out io.Writer) *TerminalPrompter {
	return &TerminalPrompter{In: in, Out: out, reader: bufio.NewReader(in)}
}

func (p *TerminalPrompter) Announce(heading string, lines ...string) {
	fmt.Fprintf(p.Out, "\n%s\n", heading)
	for _, l := range lines {
		if l == "" {
			fmt.Fprintln(p.Out)
			continue
		}
		fmt.Fprintf(p.Out, "  %s\n", l)
	}
}

func (p *TerminalPrompter) Infof(format string, args ...any) {
	fmt.Fprintf(p.Out, "  "+format+"\n", args...)
}

func (p *TerminalPrompter) readLine() (string, error) {
	line, err := p.reader.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		if errors.Is(err, io.EOF) {
			return "", ErrAborted
		}
		return "", err
	}
	return strings.TrimSpace(strings.ToLower(line)), nil
}

func (p *TerminalPrompter) Confirm(question string) (bool, error) {
	for {
		fmt.Fprintf(p.Out, "\n  %s [y/N] ", question)
		answer, err := p.readLine()
		if err != nil {
			return false, err
		}
		switch answer {
		case "y", "yes":
			return true, nil
		case "", "n", "no":
			return false, nil
		}
		fmt.Fprintln(p.Out, "  Answer y or n.")
	}
}

func (p *TerminalPrompter) Step(prompt string) (StepAction, error) {
	for {
		fmt.Fprintf(p.Out, "\n  %s\n  [enter] done   [s] skip   [q] stop: ", prompt)
		answer, err := p.readLine()
		if err != nil {
			return StepAbort, err
		}
		switch answer {
		case "":
			return StepDone, nil
		case "s", "skip":
			return StepSkip, nil
		case "q", "quit", "stop":
			return StepAbort, nil
		}
		fmt.Fprintln(p.Out, "  Press enter when done, s to skip this one, or q to stop.")
	}
}

func (p *TerminalPrompter) Choose(title string, options []string) (int, error) {
	if len(options) == 0 {
		return 0, errors.New("nothing to choose from")
	}
	for {
		fmt.Fprintf(p.Out, "\n%s\n", title)
		for i, o := range options {
			fmt.Fprintf(p.Out, "  %d) %s\n", i+1, o)
		}
		fmt.Fprintf(p.Out, "\n  Run which? [1] ")
		answer, err := p.readLine()
		if err != nil {
			return 0, err
		}
		if answer == "" {
			return 0, nil
		}
		var idx int
		if _, err := fmt.Sscanf(answer, "%d", &idx); err == nil && idx >= 1 && idx <= len(options) {
			return idx - 1, nil
		}
		fmt.Fprintf(p.Out, "  Enter a number between 1 and %d.\n", len(options))
	}
}
