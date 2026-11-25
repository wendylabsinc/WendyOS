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
	"github.com/wendylabsinc/wendy/go/world/production/livefusion"
)

func main() { os.Exit(run()) }
func run() int {
	flags := flag.NewFlagSet("world-agent-fusion-session", flag.ContinueOnError)
	path := flags.String("config", "", "absolute public-root simulation commissioning file")
	if flags.Parse(os.Args[1:]) != nil || flags.NArg() != 0 || *path == "" {
		return 2
	}
	cfg, err := livefusion.LoadConfig(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "live fusion configuration refused")
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
	emit := func(value any) error {
		raw, err := json.Marshal(value)
		if err != nil || len(raw) > livefusion.MaxResultBytes {
			return fmt.Errorf("bounded fusion result serialization failed")
		}
		_, err = os.Stdout.Write(append(raw, '\n'))
		return err
	}
	out, err := livefusion.RunNativeContinuous(ctx, cfg, appv1.NewWorldAppServiceClient(conn), func(r livefusion.TickResult) error { return emit(r) })
	if e := emit(out); e != nil {
		return 1
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fusion session:", err)
		return 1
	}
	return 0
}
