// world-replica-proof checks public test evidence, never an agent's private key.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/agentmodule"
	"github.com/wendylabsinc/wendy/go/world/production/authenticatedtime"
	"github.com/wendylabsinc/wendy/go/world/production/internal/agentpairverify"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "paired public proof refused")
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	f := flag.NewFlagSet("world-replica-proof", flag.ContinueOnError)
	expect := f.String("expectation", "", "protected public fixture expectations")
	request := f.String("request", "", "original PublishRequest protojson")
	origin := f.String("origin", "", "original acknowledged envelope")
	received := f.String("received", "", "optional exact envelope returned by peer Query")
	if f.Parse(args) != nil || f.NArg() != 0 || *expect == "" || *request == "" || *origin == "" {
		return agentpairverify.ErrRefused
	}
	paths := []string{*expect, *request, *origin}
	if *received != "" {
		paths = append(paths, *received)
	}
	values := make([][]byte, len(paths))
	for i, path := range paths {
		b, err := agentmodule.ReadCommissionedFile(path, agentpairverify.MaxInput)
		if err != nil {
			return err
		}
		values[i] = b
	}
	e, err := agentpairverify.DecodeExpectation(values[0])
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	clock, err := authenticatedtime.Acquire(ctx, authenticatedtime.Config{Servers: authenticatedtime.WendyServers(), QueryTimeout: 3 * time.Second, MaxHoldover: time.Minute, DriftPPM: 100, MaxWidth: 5 * time.Second})
	if err != nil {
		return err
	}
	var copy []byte
	if len(values) == 4 {
		copy = values[3]
	}
	r, err := agentpairverify.Verify(ctx, e, values[1], values[2], copy, clock)
	if err != nil {
		return err
	}
	r.TimeEvidence = clock.Report().EvidenceID
	return json.NewEncoder(out).Encode(r)
}
