// world-replica-probe runs synthetic scalar phases through one app World socket.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/replicaprobe"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
func run(ctx context.Context, args []string, out, stderr io.Writer) int {
	f := flag.NewFlagSet("world-replica-probe", flag.ContinueOnError)
	f.SetOutput(stderr)
	config := f.String("config", "", "protected config file")
	commands := f.String("commands", "", "protected numbered command directory")
	receipts := f.String("receipts", "", "empty protected receipt directory")
	if f.Parse(args) != nil || f.NArg() != 0 || *config == "" || *commands == "" || *receipts == "" {
		return 2
	}
	c, e := replicaprobe.LoadConfig(*config)
	if e != nil {
		fmt.Fprintln(stderr, "replica probe configuration refused")
		return 2
	}
	conn, e := replicaprobe.Dial(os.Getenv("WENDY_WORLD_SOCKET"))
	if e != nil {
		fmt.Fprintln(stderr, "commissioned World socket required")
		return 2
	}
	defer conn.Close()
	result, e := replicaprobe.Run(ctx, c, appv1.NewWorldAppServiceClient(conn), *commands, *receipts)
	if json.NewEncoder(out).Encode(result) != nil {
		return 1
	}
	if e != nil {
		return 1
	}
	return 0
}
