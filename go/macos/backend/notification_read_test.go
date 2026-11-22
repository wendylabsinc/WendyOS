package backend

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type notificationTestConn struct {
	grpc.ClientConnInterface
	row   *pb.Notification
	err   error
	calls int
}

func (c *notificationTestConn) Invoke(ctx context.Context, method string, request, response any, _ ...grpc.CallOption) error {
	c.calls++
	if method != pb.NotificationService_GetNotification_FullMethodName {
		return errors.New("wrong RPC")
	}
	r := request.(*pb.GetNotificationRequest)
	if r.GetId() != "" || r.GetNotificationId() != "a4c0899e-f2c0-4c60-a42d-5c89f756be55" {
		return errors.New("incorrect reference")
	}
	if c.err != nil {
		return c.err
	}
	proto.Merge(response.(*pb.Notification), c.row)
	return nil
}

func TestNotificationValidatesRecipientResponse(t *testing.T) {
	const org = "5c9e14bb-49c0-4178-9cc5-493a151f9885"
	const id = "a4c0899e-f2c0-4c60-a42d-5c89f756be55"
	for _, tc := range []struct {
		name string
		row  *pb.Notification
		err  error
		fail bool
	}{
		{"valid", &pb.Notification{OrganizationId: org, NotificationId: proto.String(id), DeepLink: "wendy://organizations/" + org + "/devices/0cdfb0f4-fd00-43bb-a821-ee28ed0b6cc9/live"}, nil, false},
		{"different organization", &pb.Notification{OrganizationId: "other", NotificationId: proto.String(id)}, nil, true},
		{"different notification", &pb.Notification{OrganizationId: org, NotificationId: proto.String("other")}, nil, true},
		{"deleted", &pb.Notification{OrganizationId: org, NotificationId: proto.String(id), DeletedAt: timestamppb.Now()}, nil, true},
		{"request failure", nil, errors.New("unavailable"), true},
		{"empty response", &pb.Notification{}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &notificationTestConn{row: tc.row, err: tc.err}
			n, err := readNotification(context.Background(), c, org, id)
			if (err != nil) != tc.fail {
				t.Fatalf("notification=%v error=%v", n, err)
			}
			if !tc.fail && n.GetDeepLink() != tc.row.GetDeepLink() {
				t.Fatal("lost device link")
			}
		})
	}
	c := &notificationTestConn{}
	if _, err := readNotification(context.Background(), c, "invalid", id); err == nil {
		t.Fatal("accepted malformed organization")
	}
	if _, err := readNotification(context.Background(), c, org, "invalid"); err == nil {
		t.Fatal("accepted malformed reference")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readNotification(ctx, c, org, id); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if c.calls != 0 {
		t.Fatal("invalid or cancelled request reached Cloud")
	}
}

func TestNotificationAccountScopeBeforeCloud(t *testing.T) {
	const org = "5c9e14bb-49c0-4178-9cc5-493a151f9885"
	const id = "a4c0899e-f2c0-4c60-a42d-5c89f756be55"
	for _, principal := range []string{"", "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/one"} {
		auth := &config.AuthConfig{Certificates: []config.CertificateInfo{{PrincipalURI: principal}}}
		called := false
		_, err := notificationForAccount(context.Background(), auth, org, id, func(context.Context, func(context.Context, grpc.ClientConnInterface) error) error {
			called = true
			return nil
		})
		if err == nil || called {
			t.Fatalf("wrong account reached Cloud: called=%v err=%v", called, err)
		}
	}
}

func TestNotificationDeadlineAndCancellation(t *testing.T) {
	const org = "5c9e14bb-49c0-4178-9cc5-493a151f9885"
	const id = "a4c0899e-f2c0-4c60-a42d-5c89f756be55"
	auth := &config.AuthConfig{Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + org + "/operator/one"}}}
	for _, parentLimit := range []time.Duration{time.Minute, time.Second} {
		parent, cancel := context.WithTimeout(context.Background(), parentLimit)
		defer cancel()
		conn := &notificationTestConn{row: &pb.Notification{OrganizationId: org, NotificationId: proto.String(id)}}
		var requestContext context.Context
		got, err := notificationForAccount(parent, auth, org, id, func(ctx context.Context, call func(context.Context, grpc.ClientConnInterface) error) error {
			requestContext = ctx
			deadline, ok := ctx.Deadline()
			remaining := time.Until(deadline)
			if !ok || remaining <= 0 || remaining > min(parentLimit, 20*time.Second) {
				t.Fatalf("unbounded request: %v", remaining)
			}
			return call(ctx, conn)
		})
		if err != nil || got.GetNotificationId() != id || conn.calls != 1 {
			t.Fatalf("notification=%v calls=%d err=%v", got, conn.calls, err)
		}
		if !errors.Is(requestContext.Err(), context.Canceled) {
			t.Fatal("request context leaked")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn := &notificationTestConn{}
	_, err := notificationForAccount(ctx, auth, org, id, func(ctx context.Context, call func(context.Context, grpc.ClientConnInterface) error) error {
		return call(ctx, conn)
	})
	if !errors.Is(err, context.Canceled) || conn.calls != 0 {
		t.Fatalf("cancelled request reached Cloud: %d %v", conn.calls, err)
	}
}

// Exercise the app entry point through the shared saved-account/Cloud transport,
// rather than only testing the extracted notification parser.
func TestBackendNotificationUsesSelectedAccount(t *testing.T) {
	const org = "5c9e14bb-49c0-4178-9cc5-493a151f9885"
	const id = "a4c0899e-f2c0-4c60-a42d-5c89f756be55"
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv(clouddefaults.CloudInsecureEnv, "true")
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service := &notificationTestServer{t: t, org: org, id: id}
	server := grpc.NewServer()
	pb.RegisterNotificationServiceServer(server, service)
	go server.Serve(lis)
	t.Cleanup(server.Stop)
	cfg := &config.Config{Auth: []config.AuthConfig{{Name: "selected", CloudGRPC: lis.Addr().String(), APIKey: "selected-token", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + org + "/operator/one"}}}}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	b := New()
	row, err := b.Notification("selected", org, id)
	if err != nil || row.GetNotificationId() != id {
		t.Fatalf("notification=%v err=%v", row, err)
	}
	if _, err := b.Notification("missing", org, id); err == nil {
		t.Fatal("missing account accepted")
	}
	if _, err := b.Notification("selected", "11111111-1111-4111-8111-111111111111", id); err == nil {
		t.Fatal("wrong organization accepted")
	}
	if got := service.calls.Load(); got != 1 {
		t.Fatalf("unexpected requests to Cloud: %d", got)
	}
}

type notificationTestServer struct {
	pb.UnimplementedNotificationServiceServer
	t       *testing.T
	org, id string
	calls   atomic.Int32
}

func (s *notificationTestServer) GetNotification(ctx context.Context, req *pb.GetNotificationRequest) (*pb.Notification, error) {
	s.calls.Add(1)
	md, _ := metadata.FromIncomingContext(ctx)
	if got := md.Get("authorization"); len(got) != 1 || got[0] != "Bearer selected-token" {
		s.t.Errorf("wrong account authentication: %v", got)
	}
	if req.GetNotificationId() != s.id {
		s.t.Errorf("wrong notification: %v", req)
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 20*time.Second {
		s.t.Error("notification deadline lost at app boundary")
	}
	return &pb.Notification{OrganizationId: s.org, NotificationId: proto.String(s.id)}, nil
}
