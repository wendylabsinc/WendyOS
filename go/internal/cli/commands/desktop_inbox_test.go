package commands

import (
	"context"
	"errors"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
	"io"
	"testing"
)

type inboxTestConn struct {
	grpc.ClientConnInterface
	stream *inboxTestStream
	calls  int
}

func (c *inboxTestConn) NewStream(ctx context.Context, _ *grpc.StreamDesc, method string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
	c.calls++
	if method != pb.NotificationService_ListNotifications_FullMethodName {
		return nil, errors.New("wrong RPC")
	}
	return c.stream, nil
}

type inboxTestStream struct {
	grpc.ClientStream
	rows    []*pb.Notification
	err     error
	request *pb.ListNotificationsRequest
}

func (s *inboxTestStream) SendMsg(m any) error {
	s.request = m.(*pb.ListNotificationsRequest)
	return nil
}
func (s *inboxTestStream) CloseSend() error { return nil }
func (s *inboxTestStream) RecvMsg(m any) error {
	if len(s.rows) == 0 {
		if s.err != nil {
			return s.err
		}
		return io.EOF
	}
	m.(*pb.ListNotificationsResponse).Notification = s.rows[0]
	s.rows = s.rows[1:]
	return nil
}
func TestDesktopInboxReadOnlyScopeAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []*pb.Notification
		err  error
		want int
		fail bool
	}{
		{name: "empty"},
		{name: "inbox", rows: []*pb.Notification{{OrganizationId: "org", Title: "Thermal camera above 30°C"}}, want: 1},
		{name: "deleted", rows: []*pb.Notification{{OrganizationId: "org", DeletedAt: timestamppb.Now()}}},
		{name: "wrong organization", rows: []*pb.Notification{{OrganizationId: "other"}}, fail: true},
		{name: "partial stream failure", rows: []*pb.Notification{{OrganizationId: "org"}}, err: errors.New("offline"), fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &inboxTestStream{rows: tc.rows, err: tc.err}
			conn := &inboxTestConn{stream: stream}
			rows, err := readDesktopInbox(context.Background(), conn, "org")
			if (err != nil) != tc.fail || len(rows) != tc.want {
				t.Fatalf("rows=%d err=%v", len(rows), err)
			}
			r := stream.request
			if r.GetOrganizationId() != "org" || r.GetUserId() != "" || r.GetLimit() != 100 || r.GetIncludeDeleted() {
				t.Fatalf("incorrect request: %v", r)
			}
		})
	}
}
func TestDesktopInboxCancellationAndMissingTenant(t *testing.T) {
	conn := &inboxTestConn{}
	if _, err := readDesktopInbox(context.Background(), conn, ""); err == nil {
		t.Fatal("accepted empty tenant")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readDesktopInbox(ctx, conn, "org"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if conn.calls != 0 {
		t.Fatal("request issued")
	}
}
