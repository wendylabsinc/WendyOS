package containerd

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/delivery"
)

// SetDeliverySource is wired once before serving any agent listener.
func (c *Client) SetDeliverySource(source delivery.Source) { c.deliverySource = source }
