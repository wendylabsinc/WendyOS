//go:build !world_wendy_boot

// world-wendy-lab stages an isolated, simulation-only ordinary Wendy app.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "stage" {
		fmt.Fprintln(os.Stderr, "usage: world-wendy-lab stage --out DIR --run-id ID --transport host-http --allow-insecure-simulation [--port 18881] [--arch arm64]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("stage", flag.ExitOnError)
	var opts stageOptions
	fs.StringVar(&opts.Out, "out", "", "new private output directory; never overwritten")
	fs.StringVar(&opts.Repo, "repo", ".", "private World repository root")
	fs.StringVar(&opts.RunID, "run-id", "", "unique lowercase simulation run ID, up to 32 characters")
	fs.IntVar(&opts.Port, "port", 18881, "guest HTTP/readiness port")
	fs.StringVar(&opts.Arch, "arch", "arm64", "Linux architecture: arm64 or amd64")
	fs.StringVar(&opts.Transport, "transport", "", "must explicitly select host-http for this installed CLI")
	fs.BoolVar(&opts.AllowInsecureSimulation, "allow-insecure-simulation", false, "acknowledge simulator-only cleartext HTTP fallback")
	_ = fs.Parse(os.Args[2:])
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(2)
	}
	opts.Now = time.Now
	manifest, err := stage(opts, buildBinary)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Paths and scope only. Never print signing keys or credential contents.
	fmt.Printf("Staged simulation app %s\nProject: %s/project\nClients: %s/clients\nManifest: %s/manifest.json\nCredentials expire: %s\n", manifest.AppID, manifest.Directory, manifest.Directory, manifest.Directory, time.UnixMilli(manifest.ExpiresAtUnixMS).UTC().Format(time.RFC3339))
}
