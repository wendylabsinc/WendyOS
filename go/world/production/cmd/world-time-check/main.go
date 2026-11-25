// world-time-check obtains an authenticated interval without changing a clock.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/authenticatedtime"
)

func main() {
	source := flag.String("source", "", "must explicitly select wendy")
	flag.Parse()
	if *source != "wendy" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: world-time-check --source wendy")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	p, err := authenticatedtime.Acquire(ctx, authenticatedtime.Config{Servers: authenticatedtime.WendyServers(), QueryTimeout: 3 * time.Second, MaxHoldover: 10 * time.Minute, DriftPPM: 100, MaxWidth: 5 * time.Second})
	if err != nil {
		fmt.Fprintln(os.Stderr, "authenticated time unavailable:", err)
		os.Exit(1)
	}
	if err = json.NewEncoder(os.Stdout).Encode(p.Report()); err != nil {
		os.Exit(1)
	}
}
