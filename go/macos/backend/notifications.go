package backend

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudsession"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
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

func (b *Backend) Inbox(account string) ([]*cloudpb.Notification, error) {
	ctx, done := b.begin()
	defer done()
	return commands.DesktopInbox(ctx, account)
}

func (b *Backend) Notification(account, organization, id string) (*cloudpb.Notification, error) {
	ctx, done := b.begin()
	defer done()
	auth, err := cloudsession.LoadAccount(account, "")
	if err != nil {
		return nil, err
	}
	return notificationForAccount(ctx, auth, organization, id, func(ctx context.Context, call func(context.Context, grpc.ClientConnInterface) error) error {
		return cloudsession.With(ctx, auth, commands.EnsureCloudAccessToken, call)
	})
}
