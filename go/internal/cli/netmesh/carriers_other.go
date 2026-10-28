//go:build !linux && !darwin

package netmesh

import (
	"context"
	"errors"
)

func (c *Client) Run(context.Context) error {
	c.network.Close()
	return errors.New("desktop mesh requires macOS or Linux")
}
