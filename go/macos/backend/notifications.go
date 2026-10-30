package backend

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
)

func (b *Backend) CampaignNotifications(session uint64) ([]commands.DesktopCampaignNotification, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return nil, errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	return commands.DesktopCampaignNotifications(ctx, conn)
}
func (b *Backend) SetCampaignNotifications(session uint64, name string, enabled bool) error {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	return commands.DesktopSetCampaignNotifications(ctx, conn, name, enabled)
}

func (b *Backend) PushRegistration(account string, token []byte, topic string, environment int32, enabled bool) error {
	ctx, done := b.begin()
	defer done()
	return commands.DesktopPushRegistration(ctx, account, token, topic, environment, enabled)
}
