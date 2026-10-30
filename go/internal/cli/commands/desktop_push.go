package commands

import (
	"context"
	"errors"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
)

// DesktopPushRegistration binds this installation's APNs token to the explicitly
// selected signed-in human account. Cloud derives the recipient from authentication.
func DesktopPushRegistration(ctx context.Context, account string, token []byte, topic string, environment int32, enabled bool) error {
	if err := validateDesktopPush(token, topic, environment); err != nil {
		return err
	}
	auth, err := desktopCloudAuth(account, "")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return withCloud(ctx, auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
		return setDesktopPushRegistration(ctx, conn, token, topic, environment, enabled)
	})
}

func validateDesktopPush(token []byte, topic string, environment int32) error {
	if len(token) == 0 || len(token) > 4096 {
		return errors.New("Apple did not provide a valid push token")
	}
	if topic != "dev.wendy.desktop" && topic != "dev.wendy.mobile" {
		return errors.New("unsupported Wendy application")
	}
	if environment != 1 && environment != 2 {
		return errors.New("this build is not provisioned for Apple push notifications")
	}
	return nil
}

func setDesktopPushRegistration(ctx context.Context, conn grpc.ClientConnInterface, token []byte, topic string, environment int32, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateDesktopPush(token, topic, environment); err != nil {
		return err
	}
	client := pb.NewAPNSServiceClient(conn)
	if enabled {
		response, err := client.RegisterDeviceToken(ctx, &pb.APNSRegisterDeviceTokenRequest{DeviceToken: token, Topic: topic, Environment: pb.APNSEnvironment(environment)})
		if err != nil {
			return err
		}
		if !response.GetSuccess() {
			return errors.New("Wendy Cloud did not confirm notification registration")
		}
	} else {
		response, err := client.UnregisterDeviceToken(ctx, &pb.APNSUnregisterDeviceTokenRequest{DeviceToken: token, Topic: topic, Environment: pb.APNSEnvironment(environment)})
		if err != nil {
			return err
		}
		if !response.GetSuccess() {
			return errors.New("Wendy Cloud did not confirm notification removal")
		}
	}
	return nil
}
