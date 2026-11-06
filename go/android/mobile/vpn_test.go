package mobile

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/macos/vpn"
)

func TestAutomaticVPNRefreshAddsAndRemovesDevices(t *testing.T) {
	relay, initial, err := vpn.NewRelay(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	engine, err := newVPNEngine(initial)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{engine: engine, vpnConfig: initial}
	defer func() { c.engine.Close() }()
	controller, control, err := vpn.NewRelay(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	var mu sync.Mutex
	var next vpn.Config
	controller.SetRefresh(func(ctx context.Context, online bool) (vpn.Config, error) {
		mu.Lock()
		defer mu.Unlock()
		if !online {
			t.Error("normal internet state was lost")
		}
		return next, nil
	})
	for _, routes := range [][]vpn.Route{{{Selector: "mesh:one"}, {Selector: "mesh:two"}}, nil} {
		data, cfg, err := vpn.NewRelay(routes, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer data.Close()
		mu.Lock()
		next = cfg
		mu.Unlock()
		changed, err := c.refreshVPNConfig(context.Background(), control, true)
		if err != nil {
			t.Fatal(err)
		}
		if changed == nil || len(changed.Routes) != len(routes) {
			t.Fatalf("wrong automatic routes: %+v", changed)
		}
		current := c.engine
		changed, err = c.refreshVPNConfig(context.Background(), control, true)
		if err != nil || changed != nil || c.engine != current {
			t.Fatalf("unchanged directory replaced engine: %+v %v", changed, err)
		}
	}
}

func TestCancelVPNInterruptsRefresh(t *testing.T) {
	controller, control, err := vpn.NewRelay(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	entered := make(chan struct{})
	controller.SetRefresh(func(ctx context.Context, online bool) (vpn.Config, error) {
		close(entered)
		<-ctx.Done()
		return vpn.Config{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Client{vpnContext: ctx, vpnCancel: cancel}
	done := make(chan error, 1)
	go func() { _, err := c.refreshVPNConfig(ctx, control, true); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	c.cancelVPN()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh did not cancel")
	}
}
