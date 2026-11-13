package commands

import (
	"context"
	"errors"
	"fmt"
	"testing"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
)

type pushTestConn struct {
	grpc.ClientConnInterface
	calls       int
	success     bool
	err         error
	enabled     bool
	topic       string
	environment pb.APNSEnvironment
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
	if string(token) != "apple-token" || topic != c.topic || environment != c.environment {
		return errors.New("registration changed")
	}
	return nil
}
func TestDesktopPushRegistrationConfirmedByCloud(t *testing.T) {
	for _, topic := range []string{"dev.wendy.desktop", "dev.wendy.mobile", "dev.wendy.ccr"} {
		for _, environment := range []pb.APNSEnvironment{pb.APNSEnvironment_APNS_ENVIRONMENT_DEVELOPMENT, pb.APNSEnvironment_APNS_ENVIRONMENT_PRODUCTION} {
			for _, enabled := range []bool{true, false} {
				for _, success := range []bool{true, false} {
					t.Run(fmt.Sprintf("%s/%s/enabled=%t/success=%t", topic, environment, enabled, success), func(t *testing.T) {
						conn := &pushTestConn{enabled: enabled, success: success, topic: topic, environment: environment}
						err := setDesktopPushRegistration(context.Background(), conn, []byte("apple-token"), topic, int32(environment), enabled)
						if conn.calls != 1 || (err == nil) != success {
							t.Fatalf("calls=%d err=%v", conn.calls, err)
						}
					})
				}
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
		{nil, "dev.wendy.ccr", 2}, {make([]byte, 4097), "dev.wendy.ccr", 2}, {[]byte("token"), "dev.wendy.ccr", 0},
		{[]byte("token"), "dev.wendy.ccr.beta", 2}, {[]byte("token"), "dev.wendy.ccr.evil", 2},
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
