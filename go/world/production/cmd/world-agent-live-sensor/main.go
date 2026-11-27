package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
)

func main() { os.Exit(run()) }
func run() int {
	flags := flag.NewFlagSet("world-agent-live-sensor", flag.ContinueOnError)
	path := flags.String("config", "", "absolute path to private live sensor configuration")
	if flags.Parse(os.Args[1:]) != nil || flags.NArg() != 0 || *path == "" {
		return 2
	}
	cfg, err := liveapp.LoadConfig(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "live sensor configuration refused")
		return 2
	}
	conn, err := liveapp.Dial(os.Getenv("WENDY_WORLD_SOCKET"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "explicit World service socket required")
		return 2
	}
	defer conn.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, runErr := liveapp.Run(ctx, cfg, appv1.NewWorldAppServiceClient(conn))
	raw, err := json.Marshal(result)
	if err != nil || len(raw) > liveapp.MaxResultBytes {
		fmt.Fprintln(os.Stderr, "bounded result serialization failed")
		return 1
	}
	if _, err = os.Stdout.Write(append(raw, '\n')); err != nil {
		return 1
	}
	if runErr != nil {
		return 1
	}
	return 0
}
