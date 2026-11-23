package backend

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// FleetOverview is the app-only coordinator. CLI discovery stays sequential.
func FleetOverview(ctx context.Context, account, org string) (commands.DesktopFleet, error) {
	reader, err := commands.NewDesktopFleetReader(account, org)
	if err != nil {
		return commands.DesktopFleet{}, err
	}
	assets, online, presenceErr, err := fetchFleetAssets(ctx, reader.Assets)
	if err != nil {
		return commands.DesktopFleet{}, err
	}
	return reader.Overview(assets, online, presenceErr)
}

// Fetch independent inventory and broker presence concurrently. Inventory errors
// fail the refresh; presence errors still allow the inventory to be displayed.
func fetchFleetAssets(ctx context.Context, fetch func(context.Context, bool) ([]*cloudpbv2.Asset, error)) (assets, online []*cloudpbv2.Asset, presenceErr, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		assets []*cloudpbv2.Asset
		err    error
	}
	presence := make(chan result, 1)
	go func() {
		items, e := fetch(ctx, true)
		presence <- result{items, e}
	}()
	assets, err = fetch(ctx, false)
	if err != nil {
		return nil, nil, nil, err
	}
	select {
	case r := <-presence:
		return assets, r.assets, r.err, nil
	case <-ctx.Done():
		return assets, nil, ctx.Err(), nil
	}
}
