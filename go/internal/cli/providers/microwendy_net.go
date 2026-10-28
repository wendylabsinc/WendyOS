package providers

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

type liteNetClient interface {
	GetNetStatus(time.Duration) (*litepb.WendyComNetStatus, error)
	SetNetConfig(*litepb.WendyComNetConfig, time.Duration) (*litepb.WendyComNetStatus, error)
	Close() error
}

var connectLiteNetClient = func(ctx context.Context, p *MicroWendyProvider, device models.ExternalDevice) (liteNetClient, error) {
	return p.connectClient(ctx, device)
}
var _ liteNetClient = (*liteclient.WendyLiteClient)(nil)

func (p *MicroWendyProvider) NetStatus(ctx context.Context, device models.ExternalDevice) (*litepb.WendyComNetStatus, error) {
	client, err := connectLiteNetClient(ctx, p, device)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	return client.GetNetStatus(10 * time.Second)
}

func (p *MicroWendyProvider) ConfigureNet(ctx context.Context, device models.ExternalDevice, update func(*litepb.WendyComNetConfig) error) (*litepb.WendyComNetStatus, error) {
	client, err := connectLiteNetClient(ctx, p, device)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	status, err := client.GetNetStatus(10 * time.Second)
	if err != nil {
		return nil, err
	}
	config := status.GetDesired()
	if config == nil {
		config = &litepb.WendyComNetConfig{}
	}
	if err := update(config); err != nil {
		return nil, err
	}
	return client.SetNetConfig(config, 10*time.Second)
}
