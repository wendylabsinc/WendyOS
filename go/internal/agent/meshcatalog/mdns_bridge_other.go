//go:build !linux

package meshcatalog

import (
	"context"
	"errors"
)

func (b *MDNSBridge) Run(context.Context) error {
	return errors.New("isolated app mDNS bridge requires Linux")
}
