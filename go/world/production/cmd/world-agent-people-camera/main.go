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
	"github.com/wendylabsinc/wendy/go/world/production/peoplecamera"
)

func main() { os.Exit(run()) }
func run() int {
	flags := flag.NewFlagSet("world-agent-people-camera", flag.ContinueOnError)
	path := flags.String("config", "", "absolute camera commissioning file")
	if flags.Parse(os.Args[1:]) != nil || flags.NArg() != 0 || *path == "" {
		return 2
	}
	cfg, err := peoplecamera.LoadConfig(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "camera configuration refused")
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
	out := json.NewEncoder(os.Stdout)
	err = peoplecamera.Run(ctx, cfg, appv1.NewWorldAppServiceClient(conn), func(r liveapp.Receipt) error { return out.Encode(r) })
	if err != nil {
		fmt.Fprintln(os.Stderr, "camera publisher stopped:", err)
		return 1
	}
	return 0
}
