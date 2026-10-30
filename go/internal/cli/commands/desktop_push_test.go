package commands

import (
	"context"
	"errors"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"testing"
)

type pushTestConn struct {
	grpc.ClientConnInterface
	calls   int
	success bool
	err     error
	enabled bool
}

func (c *pushTestConn) Invoke(ctx context.Context, method string, in, out any, _ ...grpc.CallOption) error {
	c.calls++
	if c.err != nil {
		return c.err
	}
	var token []byte
	var topic string
	var environment pb.APNSEnvironment
	if c.enabled {
		if method != pb.APNSService_RegisterDeviceToken_FullMethodName {
			return errors.New("wrong RPC")
		}
		r := in.(*pb.APNSRegisterDeviceTokenRequest)
		token = r.DeviceToken
		topic = r.Topic
		environment = r.Environment
		out.(*pb.APNSRegisterDeviceTokenResponse).Success = c.success
	} else {
		if method != pb.APNSService_UnregisterDeviceToken_FullMethodName {
			return errors.New("wrong RPC")
		}
		r := in.(*pb.APNSUnregisterDeviceTokenRequest)
		token = r.DeviceToken
		topic = r.Topic
		environment = r.Environment
		out.(*pb.APNSUnregisterDeviceTokenResponse).Success = c.success
	}
	if string(token) != "apple-token" || topic != "dev.wendy.mobile" || environment != pb.APNSEnvironment_APNS_ENVIRONMENT_DEVELOPMENT {
		return errors.New("registration changed")
	}
	return nil
}
func TestDesktopPushRegistrationConfirmedByCloud(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, success := range []bool{true, false} {
			conn := &pushTestConn{enabled: enabled, success: success}
			err := setDesktopPushRegistration(context.Background(), conn, []byte("apple-token"), "dev.wendy.mobile", 1, enabled)
			if conn.calls != 1 || (err == nil) != success {
				t.Fatalf("enabled=%v success=%v err=%v", enabled, success, err)
			}
		}
	}
	conn := &pushTestConn{enabled: true, err: errors.New("cloud unavailable")}
	if err := setDesktopPushRegistration(context.Background(), conn, []byte("apple-token"), "dev.wendy.mobile", 1, true); err == nil {
		t.Fatal("ignored Cloud error")
	}
}
func TestDesktopPushRejectsInvalidAndCancelled(t *testing.T) {
	for _, item := range []struct {
		token []byte
		topic string
		env   int32
	}{
		{nil, "dev.wendy.mobile", 1}, {[]byte("token"), "another.app", 1}, {[]byte("token"), "dev.wendy.mobile", 0},
	} {
		conn := &pushTestConn{}
		if err := setDesktopPushRegistration(context.Background(), conn, item.token, item.topic, item.env, true); err == nil || conn.calls != 0 {
			t.Fatal("invalid registration reached Cloud")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn := &pushTestConn{}
	if err := setDesktopPushRegistration(ctx, conn, []byte("token"), "dev.wendy.mobile", 1, true); err == nil || conn.calls != 0 {
		t.Fatal("cancelled registration reached Cloud")
	}
}
