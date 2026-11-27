// world-go2-bind makes unsigned read-only observations of an explicitly pinned
// local Go2 simulator. It has no command, reset, project-apply or credential API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/wendylabsinc/wendy/go/world/internal/go2project"
)

type options struct {
	phase, packagePath, projectDir, conversion, out, binding string
	pins                                                     go2project.Pins
}

func parse(args []string) (options, error) {
	var o options
	if len(args) == 0 || (args[0] != "bind" && args[0] != "check") {
		return o, errors.New("bind or check subcommand required")
	}
	o.phase = args[0]
	f := flag.NewFlagSet("world-go2-bind", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&o.packagePath, "package", "", "original World package root")
	f.StringVar(&o.projectDir, "project-dir", "", "exact projected runtime directory")
	f.StringVar(&o.conversion, "conversion-report", "", "converter report")
	f.StringVar(&o.out, "out", "", "new immutable public report path")
	f.StringVar(&o.binding, "binding", "", "prior observational binding; check only")
	f.StringVar(&o.pins.Endpoint, "endpoint", "", "explicit numeric loopback HTTP origin")
	f.StringVar(&o.pins.VMName, "vm-name", "", "expected managed VM name")
	f.StringVar(&o.pins.SourceDigest, "source-digest", "", "expected runtime sha256 pin")
	f.StringVar(&o.pins.ProjectID, "project-id", "", "expected linked project ID")
	f.StringVar(&o.pins.ProjectRevision, "project-revision", "", "expected runtime project revision")
	f.Uint64Var(&o.pins.WorldEpoch, "world-epoch", 0, "expected current runtime epoch")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || o.packagePath == "" || o.projectDir == "" || o.conversion == "" || o.out == "" {
		return o, errors.New("explicit package, project directory, conversion report and new output required")
	}
	if o.phase == "bind" {
		if o.binding != "" {
			return o, errors.New("bind cannot replace a prior binding")
		}
		endpoint, err := go2project.Endpoint(o.pins.Endpoint)
		if err != nil {
			return o, err
		}
		o.pins.Endpoint = endpoint
		if o.pins.Validate() != nil {
			return o, errors.New("bind requires explicit complete runtime pins")
		}
	} else if o.binding == "" || o.pins != (go2project.Pins{}) {
		return o, errors.New("check requires prior binding and refuses pin overrides")
	}
	return o, nil
}
func run(ctx context.Context, args []string, output io.Writer) error {
	o, err := parse(args)
	if err != nil {
		return err
	}
	if o.phase == "bind" {
		b, err := go2project.Bind(ctx, o.packagePath, o.projectDir, o.conversion, o.pins)
		if err != nil {
			return err
		}
		if err = go2project.WriteNew(o.out, b); err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(b)
	}
	result, checkErr := go2project.CheckBinding(ctx, o.binding, o.packagePath, o.projectDir, o.conversion)
	if err = go2project.WriteNew(o.out, result); err != nil {
		return err
	}
	if err = json.NewEncoder(output).Encode(result); err != nil {
		return err
	}
	return checkErr
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Go2 observational binding refused:", err)
		os.Exit(1)
	}
}
