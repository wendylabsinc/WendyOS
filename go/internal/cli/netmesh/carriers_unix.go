//go:build linux || darwin

package netmesh

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/bleprovider"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/nanprovider"
)

func (c *Client) Run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var wg sync.WaitGroup
	hints := &lanHints{}
	wg.Add(1)
	go func() { defer wg.Done(); hints.run(ctx, c.credentials.Device) }()
	start := func(name string, run func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				err := run(ctx)
				if err != nil {
					c.carrierStatus(name)(localmesh.CarrierStatus{Err: err})
				}
				timer := time.NewTimer(5 * time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
	selection := localmesh.NewPeerSelection(c.node.Snapshot)
	start("LAN", func(ctx context.Context) error {
		return localmesh.RunLAN(ctx, localmesh.LANConfig{Credentials: c.credentials, Node: c.node, Ethernet: true, InfrastructureWiFi: true, Selection: selection, Status: c.carrierStatus("LAN"), OperatorCandidates: hints.snapshot})
	})
	start("BLE", func(ctx context.Context) error {
		return bleprovider.Run(ctx, bleprovider.Config{Credentials: c.credentials, Node: c.node, NetworkName: fmt.Sprintf("tenant:%s:default", c.credentials.Device.Tenant()), Selection: selection, Status: c.carrierStatus("BLE")})
	})
	start("Wi-Fi Aware", func(ctx context.Context) error {
		return (nanprovider.Provider{Credentials: c.credentials, Node: c.node, Selection: selection, Status: c.carrierStatus("Wi-Fi Aware")}).Run(ctx)
	})
	err := c.node.Run()
	cancel()
	wg.Wait()
	return err
}
