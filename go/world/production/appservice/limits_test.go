package appservice

import (
	"context"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRateLimitIsPerServiceNotPerConnectionOrApp(t *testing.T) {
	f := setup(t, func(cfg *Config) { cfg.Limits.MessagesPerSecond = .0001; cfg.Limits.MessageBurst = 2 })
	a := f.mount(t, f.scope)
	bScope := f.scope
	bScope.Service = "detector-b"
	b := f.mount(t, bScope)
	aClient, _ := dial(t, a.SocketPath)
	publish(t, aClient)
	secondConnection, _ := dial(t, a.SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := secondConnection.Publish(ctx, request()); status.Code(err) != codes.ResourceExhausted {
		t.Fatal("new connection reset service limit", err)
	}
	bClient, _ := dial(t, b.SocketPath)
	publish(t, bClient)
	if f.count() != 2 {
		t.Fatal("same-app service inherited another service's budget")
	}
}

func TestConcurrentRPCBudgetSharedByConnections(t *testing.T) {
	entered := make(chan struct{})
	resume := make(chan struct{})
	f := setup(t, func(cfg *Config) {
		cfg.Limits.MaxConcurrentRPCs = 1
		accept := cfg.Accept
		cfg.Accept = func(ctx context.Context, scope serviceorigin.Scope, record serviceorigin.SignedRecord, fence *Fence) error {
			close(entered)
			select {
			case <-resume:
			case <-ctx.Done():
				return ctx.Err()
			}
			return accept(ctx, scope, record, fence)
		}
	})
	mount := f.mount(t, f.scope)
	first, _ := dial(t, mount.SocketPath)
	second, _ := dial(t, mount.SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	firstDone := make(chan error, 1)
	go func() { _, err := first.Publish(ctx, request()); firstDone <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first request did not enter")
	}
	if _, err := second.Publish(ctx, request()); status.Code(err) != codes.ResourceExhausted {
		t.Fatal("second connection bypassed RPC limit", err)
	}
	close(resume)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestConnectionBudgetClosesExcessConnections(t *testing.T) {
	f := setup(t, func(cfg *Config) { cfg.Limits.MaxConnections = 1 })
	mount := f.mount(t, f.scope)
	first, _ := dial(t, mount.SocketPath)
	publish(t, first)
	second, _ := dial(t, mount.SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := second.Publish(ctx, request()); err == nil {
		t.Fatal("excess connection admitted")
	}
	if f.count() != 1 {
		t.Fatal("connection budget exceeded before acceptance")
	}
}
