package commands

import (
	"context"
	"errors"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"testing"
	"time"
)

func TestFetchFleetAssetsConcurrent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	inventoryStarted, presenceStarted := make(chan struct{}), make(chan struct{})
	assets, online, presenceErr, err := fetchFleetAssets(ctx, func(ctx context.Context, online bool) ([]*cloudpbv2.Asset, error) {
		started, other := inventoryStarted, presenceStarted
		if online {
			started, other = presenceStarted, inventoryStarted
		}
		close(started)
		select {
		case <-other:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		id := "inventory"
		if online {
			id = "online"
		}
		return []*cloudpbv2.Asset{{Id: id}}, nil
	})
	if err != nil || presenceErr != nil {
		t.Fatalf("inventory=%v presence=%v", err, presenceErr)
	}
	if assets[0].Id != "inventory" || online[0].Id != "online" {
		t.Fatalf("results crossed: %v %v", assets, online)
	}
}

func TestFetchFleetAssetsPresenceFailureKeepsInventory(t *testing.T) {
	unavailable := errors.New("presence unavailable")
	assets, _, presenceErr, err := fetchFleetAssets(context.Background(), func(_ context.Context, online bool) ([]*cloudpbv2.Asset, error) {
		if online {
			return nil, unavailable
		}
		return []*cloudpbv2.Asset{{Id: "device"}}, nil
	})
	if err != nil || !errors.Is(presenceErr, unavailable) || len(assets) != 1 {
		t.Fatalf("assets=%v presence=%v err=%v", assets, presenceErr, err)
	}
}

func TestFetchFleetAssetsInventoryFailureCancelsPresence(t *testing.T) {
	unavailable := errors.New("inventory unavailable")
	cancelled := make(chan struct{})
	_, _, _, err := fetchFleetAssets(context.Background(), func(ctx context.Context, online bool) ([]*cloudpbv2.Asset, error) {
		if !online {
			return nil, unavailable
		}
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	if !errors.Is(err, unavailable) {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("presence was not cancelled")
	}
}
