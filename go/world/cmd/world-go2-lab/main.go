// world-go2-lab creates a private, simulation-only ordinary Wendy app.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
)

func run(args []string) error {
	if len(args) > 0 && args[0] == "release" {
		f := flag.NewFlagSet("release", flag.ContinueOnError)
		var o releaseOptions
		f.StringVar(&o.Out, "out", "", "new release output directory")
		f.StringVar(&o.Repo, "repo", "", "private source repository")
		f.StringVar(&o.Arch, "arch", "arm64", "pinned Linux platform")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		if f.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		m, e := release(o, buildBinary)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(m)
	}
	if len(args) > 0 && args[0] == "validate-run" {
		f := flag.NewFlagSet("validate-run", flag.ContinueOnError)
		var dir, bundle, release, image string
		var current bool
		f.StringVar(&dir, "directory", "", "bounded private snapshot")
		f.StringVar(&bundle, "bundle-sha256", "", "expected exact manifest digest")
		f.StringVar(&release, "release-id", "", "expected code inventory digest")
		f.StringVar(&image, "image-digest", "", "externally commissioned OCI digest")
		f.BoolVar(&current, "current", false, "require current original grant validity")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		if f.NArg() != 0 || dir == "" {
			return errors.New("explicit run validation inputs required")
		}
		if e := validateRun(dir, bundle, release, image, current, nil); e != nil {
			return errors.New("run_validation_refused")
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"schema": "wendy.world.go2-run-validation.v1", "domain": "simulation", "valid": true, "current": current})
	}
	if len(args) == 0 || (args[0] != "stage" && args[0] != "commission") {
		return errors.New("usage: world-go2-lab release|commission|stage|validate-run; each requires explicit private inputs")
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	var o options
	var releaseDir, image string
	if args[0] == "commission" {
		f.StringVar(&releaseDir, "release", "", "verified release directory")
		f.StringVar(&image, "image", "", "explicit private registry/repository@sha256 manifest reference")
	}
	f.StringVar(&o.Out, "out", "", "new private output directory")
	f.StringVar(&o.Repo, "repo", "", "private World repository")
	f.StringVar(&o.Binding, "binding", "", "explicit commissioned Go2 binding JSON")
	f.StringVar(&o.Alignment, "alignment", "", "exact commissioned alignment JSON")
	f.StringVar(&o.Profile, "profile", "", "exact commissioned profile JSON")
	f.StringVar(&o.Site, "site", "", "explicit simulation site")
	f.StringVar(&o.TaskID, "task-id", "", "one selected task ID")
	f.IntVar(&o.Port, "port", 18885, "gateway HTTP port")
	f.IntVar(&o.BridgePort, "bridge-port", 18886, "numeric loopback bridge port")
	f.StringVar(&o.Arch, "arch", "arm64", "Linux architecture; pinned base currently supports arm64 only")
	f.StringVar(&o.Transport, "transport", "", "explicit host-http simulation transport")
	f.BoolVar(&o.Insecure, "allow-insecure-simulation", false, "acknowledge unauthenticated HTTP transport with signed simulation requests")
	f.BoolVar(&o.Commission, "commission-synthetic-flat-floor", false, "explicitly commission synthetic flat floor with vertical obstacles")
	if e := f.Parse(args[1:]); e != nil {
		return e
	}
	if f.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if args[0] == "commission" {
		m, e := commission(o, releaseDir, image)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(m)
	}
	m, e := stage(o, buildBinary)
	if e != nil {
		return e
	}
	return json.NewEncoder(os.Stdout).Encode(m)
}
func main() {
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
