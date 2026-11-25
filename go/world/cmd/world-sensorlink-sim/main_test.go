package main

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDropChunkSyntax(t *testing.T) {
	for _, value := range []string{"1", "-1:0", "0:-1", "4294967296:0", "1:2:3", "a:2"} {
		if _, err := parseDrop(value); err == nil {
			t.Fatal("accepted", value)
		}
	}
	got, err := parseDrop("1:2")
	if err != nil || got.FrameSequence != 1 || got.ChunkSequence != 2 {
		t.Fatal(got, err)
	}
}
func TestPublicListenRequiresExplicitSimulationFlag(t *testing.T) {
	err := run(context.Background(), []string{"--addr", "0.0.0.0:50060", "--anchor-unix-ms", strconv.FormatInt(time.Now().UnixMilli(), 10)}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "allow-network-simulation") {
		t.Fatal(err)
	}
}
