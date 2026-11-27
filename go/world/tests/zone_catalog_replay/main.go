// zone_catalog_replay submits one retained origin proof to an authenticated local
// simulation node with fresh transport authorization. Acceptance uses it to
// distinguish record replay protection from HTTP nonce rejection.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 3 {
		panic("credentials and original record required")
	}
	b, e := os.ReadFile(os.Args[1])
	if e != nil {
		panic(e)
	}
	var cfg client.Config
	if e = authority.DecodeStrict(b, &cfg); e != nil {
		panic(e)
	}
	u, e := url.Parse(cfg.URL)
	if e != nil || u.Scheme != "http" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || cfg.Grant.Grant.Domain != "simulation" {
		panic("only authenticated loopback simulation allowed")
	}
	b, e = os.ReadFile(os.Args[2])
	if e != nil {
		panic(e)
	}
	var record authority.SignedRecord
	if e = authority.DecodeStrict(b, &record); e != nil {
		panic(e)
	}
	b, e = json.Marshal(record)
	if e != nil {
		panic(e)
	}
	c, e := client.New(cfg)
	if e != nil {
		panic(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var result json.RawMessage
	if e = c.Do(ctx, "publish", http.MethodPost, "/v1/components", b, &result); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	fmt.Println(string(result))
}
