package client

import (
	"context"
	"net/http"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// PublishPackage requires the origin to durably hold all verified local package
// bytes before publishing its signed site record. Plain Publish intentionally
// remains usable for site metadata independent of local asset availability.
func (c *Client) PublishPackage(ctx context.Context, component world.Component) error {
	prepared, err := c.PreparePublication(component)
	if err != nil {
		return err
	}
	return c.Do(ctx, "publish", http.MethodPost, "/v1/packages/publish", prepared.body, nil)
}
