package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
)

func main() {
	credentials := flag.String("credentials", "", "private client configuration")
	flag.Parse()
	if *credentials == "" {
		fmt.Fprintln(os.Stderr, "credentials are required")
		os.Exit(1)
	}
	f, e := os.Open(*credentials)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	var cfg client.Config
	d := json.NewDecoder(io.LimitReader(f, 65536))
	d.DisallowUnknownFields()
	e = d.Decode(&cfg)
	_ = f.Close()
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	c, e := client.New(cfg)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, e := c.Query(ctx)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	if e = json.NewEncoder(os.Stdout).Encode(snapshot); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
