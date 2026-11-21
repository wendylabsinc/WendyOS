package backend

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
)

// notificationForAccount resolves a push reference as the signed-in recipient.
// The payload's organization is checked against both the session and response.
func notificationForAccount(ctx context.Context, auth *config.AuthConfig, organization, id string, withCloud func(context.Context, func(context.Context, grpc.ClientConnInterface) error) error) (*pb.Notification, error) {
	if len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() != organization {
		return nil, errors.New("Switch to the notification's organization and try again")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var notification *pb.Notification
	err := withCloud(ctx, func(ctx context.Context, conn grpc.ClientConnInterface) error {
		var err error
		notification, err = readNotification(ctx, conn, organization, id)
		return err
	})
	return notification, err
}

func readNotification(ctx context.Context, conn grpc.ClientConnInterface, organization, id string) (*pb.Notification, error) {
	if _, err := uuid.Parse(organization); err != nil {
		return nil, errors.New("Invalid notification organization")
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, errors.New("Invalid notification reference")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n, err := pb.NewNotificationServiceClient(conn).GetNotification(ctx, &pb.GetNotificationRequest{NotificationId: &id})
	if err != nil {
		return nil, err
	}
	if n == nil || n.GetNotificationId() != id || n.GetOrganizationId() != organization || n.GetDeletedAt() != nil {
		return nil, errors.New("Notification is unavailable in this organization")
	}
	return n, nil
}
