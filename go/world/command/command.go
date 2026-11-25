// Package command implements the World command line without starting another
// process or taking ownership of the caller's streams and cancellation signals.
package command

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
)

// Streams are owned by the caller. Commands currently read explicit files rather
// than stdin, so credentials and task specifications cannot arrive accidentally
// from an interactive conversation or a pipe.
type Streams struct {
	Out io.Writer
	Err io.Writer
}

type invocation struct {
	ctx    context.Context
	stdout io.Writer
	stderr io.Writer
	help   bool
}

func (inv *invocation) parseFlags(flags *flag.FlagSet, args []string) error {
	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		inv.help = true
	}
	return err
}

const Help = `Manage World observations, evidence, sites and reviewed missions.

Usage: wendy world <command> [arguments]

Commands:
  query, watch       Read bounded, selected observations
  area               Fetch and inspect complete offline snapshots
  replica            Configure and inspect replica subscriptions
  frame              Convert and project geographic coordinates
  site               Publish, list and fetch signed site documents
  package            Publish, fetch and cache verified world packages
  evidence           Fetch, mirror, cache, pin and inspect retained evidence
  annotation         Read, correct and withdraw author-owned notes
  zone               Publish and inspect signed zone catalogs
  task               Review proposals, send signed tasks, stop and read history

Use a command's --help for its arguments. Explicit credentials select the
gateway and authority. These World command paths currently use simulation
credentials; a simulation result never authorizes physical movement.
`

// Run executes one command. It returns errors to its host, propagates host
// cancellation to requests, and never exits the process or replaces global IO.
func Run(ctx context.Context, args []string, streams Streams) error {
	if ctx == nil {
		return errors.New("world command requires a context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if streams.Out == nil {
		streams.Out = io.Discard
	}
	if streams.Err == nil {
		streams.Err = io.Discard
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprint(streams.Out, Help)
		return err
	}
	inv := &invocation{ctx: ctx, stdout: streams.Out, stderr: streams.Err}
	err := inv.run(args)
	if inv.help || errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}
