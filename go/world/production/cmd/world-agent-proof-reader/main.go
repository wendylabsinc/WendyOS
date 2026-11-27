package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"github.com/wendylabsinc/wendy/go/world/production/proofreader"
	"os"
	"os/signal"
	"syscall"
)

func main() { os.Exit(run()) }
func run() int {
	flags := flag.NewFlagSet("world-agent-proof-reader", flag.ContinueOnError)
	path := flags.String("config", "", "absolute commissioned expected-proof file")
	if flags.Parse(os.Args[1:]) != nil || flags.NArg() != 0 || *path == "" {
		return 2
	}
	cfg, err := proofreader.LoadConfig(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "historical proof configuration refused")
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
	out, err := proofreader.Run(ctx, cfg, appv1.NewWorldAppServiceClient(conn))
	raw, e := json.Marshal(out)
	if e != nil || len(raw) > proofreader.MaxResultBytes {
		fmt.Fprintln(os.Stderr, "bounded historical receipt serialization failed")
		return 1
	}
	if _, e = os.Stdout.Write(append(raw, '\n')); e != nil {
		return 1
	}
	if err != nil {
		return 1
	}
	return 0
}
