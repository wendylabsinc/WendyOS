package liteclient

import (
	"context"
	"time"

	wendy "github.com/wendylabsinc/wendy-go"
)

// SyncTime relays signed evidence; firmware verifies signatures and consensus.
func (c *WendyLiteClient) SyncTime(ctx context.Context) (time.Time, error) {
	return c.Client.SyncTime(ctx, wendy.GoLiteTimeSource{})
}
func (c *WendyLiteClient) SyncTimeChallenge(ctx context.Context, nonce string) (time.Time, error) {
	return c.Client.SyncTimeChallenge(ctx, nonce, wendy.GoLiteTimeSource{})
}
