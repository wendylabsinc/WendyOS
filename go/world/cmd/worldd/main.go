// worldd is the compatibility entry point for the embedded World gateway.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/wendylabsinc/wendy/go/world/gateway"
)

func main() {
	path := flag.String("config", "", "private node configuration JSON")
	flag.Parse()
	if *path == "" {
		log.Fatal("--config is required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := gateway.Run(ctx, *path); err != nil {
		log.Fatal(err)
	}
}
