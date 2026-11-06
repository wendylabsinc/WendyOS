package main

import (
	"context"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestUDPMessageObserverCancelsAlongsideManagerClose(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	manager := udpmessage.NewManager()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { logUDPMessageState(ctx, zap.New(core), manager); close(done) }()
	cancel()
	manager.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("observer shutdown waited on manager")
	}
	entries := logs.FilterMessage("wendy net: UDP message limits").All()
	if len(entries) != 1 {
		t.Fatal("missing fixed configured limits")
	}
	fields := entries[0].ContextMap()
	if fields["message_max"] != int64(65507) || fields["chunk_max"] != int64(1000) || fields["protocol_version"] != int64(2) || fields["manager_epoch"] == "" {
		t.Fatal(fields)
	}
}
