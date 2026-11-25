package main

import (
	"bytes"
	"context"
	"testing"
)

func TestExplicitFileArgumentsOnly(t *testing.T) {
	for _, args := range [][]string{nil, {"--config", "/config/probe.json"}, {"--config", "/x", "--commands", "/y", "--receipts", "/z", "extra"}, {"--key", "forbidden"}} {
		var out, stderr bytes.Buffer
		if code := run(context.Background(), args, &out, &stderr); code != 2 || out.Len() != 0 {
			t.Fatal("invalid command accepted", code, args)
		}
	}
}
