package commands

import (
	"context"
	"errors"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"io"
	"time"
)

// DesktopInbox reads the selected signed-in user's inbox. It never registers
// push tokens or changes notification/read state. Cloud derives the recipient.
func DesktopInbox(ctx context.Context, account string) ([]*pb.Notification, error) {
	auth, err := desktopCloudAuth(account, "")
	if err != nil {
		return nil, err
	}
	if len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() == "" {
		return nil, errors.New("Sign in to an organization first")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var rows []*pb.Notification
	err = withCloud(ctx, auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
		var e error
		rows, e = readDesktopInbox(ctx, conn, auth.Certificates[0].TenantUUID())
		return e
	})
	return rows, err
}

func readDesktopInbox(ctx context.Context, conn grpc.ClientConnInterface, organization string) ([]*pb.Notification, error) {
	if organization == "" {
		return nil, errors.New("Missing inbox organization")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := int32(100)
	stream, err := pb.NewNotificationServiceClient(conn).ListNotifications(ctx, &pb.ListNotificationsRequest{OrganizationId: organization, Limit: &limit})
	if err != nil {
		return nil, err
	}
	rows := make([]*pb.Notification, 0)
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
		n := response.GetNotification()
		if n == nil || n.GetDeletedAt() != nil {
			continue
		}
		if n.GetOrganizationId() != organization {
			return nil, errors.New("Notification returned for another organization")
		}
		rows = append(rows, n)
		if len(rows) >= int(limit) {
			return rows, nil
		}
	}
}
