package sessionbroker

import (
	"context"
	"net"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const operatorInternetMethod = "/wendy.sessionbroker.InternetV1/Open"

func OpenOperatorInternet(ctx context.Context, selector string) (net.Conn, error) {
	client, err := operatorControl(ctx, selector)
	if err != nil {
		return nil, err
	}
	conn, err := openOperatorStream(ctx, client.Conn, selector, operatorInternetMethod, nil, client.Close)
	if err != nil {
		client.Close()
	}
	return conn, err
}
func (a *operatorAttachment) OpenInternet(ctx context.Context, peer meshid.Device) (net.Conn, error) {
	current, err := a.snapshot()
	if err != nil {
		return nil, err
	}
	return current.session.OpenInternet(ctx, peer)
}
func (r *operatorTargets) localInternet(stream grpc.ServerStream, a *activity) (bool, error) {
	method, _ := grpc.MethodFromServerStream(stream)
	if method != operatorInternetMethod {
		return false, nil
	}
	a.activeRPCs.Add(1)
	a.touch()
	defer func() { a.activeRPCs.Add(-1); a.touch() }()
	var request rawMessage
	if err := stream.RecvMsg(&request); err != nil {
		return true, err
	}
	if len(request) != 0 {
		return true, status.Error(codes.InvalidArgument, "internet open takes no payload")
	}
	info, err := r.managementInfo(stream.Context())
	if err != nil {
		return true, err
	}
	peer, matched, err := ParseMeshSelector(info.Selector)
	if err != nil || !matched {
		return true, status.Error(codes.InvalidArgument, "invalid mesh gateway")
	}
	r.mu.Lock()
	if r.closed || r.internetActive || r.internetOpen == nil {
		r.mu.Unlock()
		return true, status.Error(codes.Unavailable, "mesh internet is unavailable or already in use")
	}
	r.internetActive = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.internetActive = false; r.mu.Unlock() }()
	setup, cancel := context.WithTimeout(stream.Context(), 90*time.Second)
	conn, err := r.internetOpen(setup, peer)
	cancel()
	if err != nil {
		return true, err
	}
	defer conn.Close()
	stop := context.AfterFunc(stream.Context(), func() { conn.Close() })
	defer stop()
	ready := rawMessage{}
	if err = stream.SendMsg(&ready); err != nil {
		return true, err
	}
	return true, relayOperatorLite(stream, conn)
}
