package backend

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
)

func (b *Backend) ManageDevice(session uint64, account, organization, device, name string, remove bool) error {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	return commands.DesktopManageDevice(ctx, conn, account, organization, device, name, remove)
}
