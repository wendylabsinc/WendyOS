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
	"github.com/wendylabsinc/wendy/go/world/production/hazardreader"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
)

func main() { os.Exit(run()) }
func run() int {
	flags := flag.NewFlagSet("world-agent-hazard-reader", flag.ContinueOnError)
	path := flags.String("config", "", "absolute public-root reader commissioning file")
	if flags.Parse(os.Args[1:]) != nil || flags.NArg() != 0 || *path == "" {
		return 2
	}
	cfg, err := hazardreader.LoadConfig(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hazard reader configuration refused")
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
	encoder := json.NewEncoder(os.Stdout)
	summary, err := hazardreader.Run(ctx, cfg, appv1.NewWorldAppServiceClient(conn), func(r hazardreader.Result) error { return encoder.Encode(r) })
	if err != nil {
		fmt.Fprintln(os.Stderr, "hazard reader stopped:", err)
	}
	if e := encoder.Encode(struct {
		Summary hazardreader.Summary `json:"summary"`
	}{summary}); e != nil || err != nil {
		return 1
	}
	return 0
}
