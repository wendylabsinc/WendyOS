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
	"github.com/wendylabsinc/wendy/go/world/production/livesensorlink"
)

func main() { os.Exit(run()) }
func run() int {
	flags := flag.NewFlagSet("world-agent-live-sensorlink", flag.ContinueOnError)
	mode := flags.String("mode", "", "source, bridge or native-bridge")
	path := flags.String("config", "", "absolute path to simulation service configuration")
	if flags.Parse(os.Args[1:]) != nil || flags.NArg() != 0 || *path == "" || (*mode != "source" && *mode != "bridge" && *mode != "native-bridge") {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var result any
	var err error
	if *mode == "source" {
		cfg, e := livesensorlink.LoadSourceConfig(*path)
		if e != nil {
			fmt.Fprintln(os.Stderr, "live SensorLink source configuration refused")
			return 2
		}
		result, err = livesensorlink.RunSource(ctx, cfg)
	} else if *mode == "native-bridge" {
		cfg, e := livesensorlink.LoadNativeBridgeConfig(*path)
		if e != nil {
			fmt.Fprintln(os.Stderr, "native SensorLink bridge configuration refused")
			return 2
		}
		conn, e := liveapp.Dial(os.Getenv("WENDY_WORLD_SOCKET"))
		if e != nil {
			fmt.Fprintln(os.Stderr, "explicit World service socket required")
			return 2
		}
		defer conn.Close()
		result, err = livesensorlink.RunNativeBridge(ctx, cfg, appv1.NewWorldAppServiceClient(conn))
	} else {
		cfg, e := livesensorlink.LoadBridgeConfig(*path)
		if e != nil {
			fmt.Fprintln(os.Stderr, "live SensorLink bridge configuration refused")
			return 2
		}
		conn, e := liveapp.Dial(os.Getenv("WENDY_WORLD_SOCKET"))
		if e != nil {
			fmt.Fprintln(os.Stderr, "explicit World service socket required")
			return 2
		}
		defer conn.Close()
		result, err = livesensorlink.RunBridge(ctx, cfg, appv1.NewWorldAppServiceClient(conn))
	}
	raw, e := json.Marshal(result)
	if e != nil || len(raw) > livesensorlink.MaxResultBytes {
		fmt.Fprintln(os.Stderr, "bounded SensorLink result serialization failed")
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
