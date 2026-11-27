// world-service-init commissions empty local publication storage. It does not
// read a device key, start an app, enroll a device or authorize hardware motion.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/agentmodule"
)

func main() {
	path := flag.String("config", "", "absolute path to the commissioned private World configuration")
	flag.Parse()
	if *path == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: world-service-init --config /absolute/path/world.json")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, 20*time.Second)
	defer timeout()
	if err := agentmodule.Initialize(ctx, *path); err != nil {
		fmt.Fprintln(os.Stderr, "World journal initialization refused:", err)
		os.Exit(1)
	}
	fmt.Println("World publication journal initialized; normal agent startup opens existing state.")
}
