package backend

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// OpenApp uses the agent's declared HTTP entitlement, never an arbitrary port
// supplied by the UI. Tunnels belong to the selected device session.
func (b *Backend) OpenApp(app string, session uint64) (string, error) {
	ctx, done := b.begin()
	defer done()
	conn, err := b.connection(session)
	if err != nil {
		return "", err
	}
	apps, err := listDesktopApps(ctx, conn)
	if err != nil {
		return "", err
	}
	var port uint32
	for _, candidate := range apps {
		if candidate.GetAppName() != app {
			continue
		}
		if candidate.GetRunningState() != pb.AppRunningState_RUNNING {
			return "", fmt.Errorf("start %s before opening its webpage", app)
		}
		port = candidate.GetHttpPort()
		break
	}
	if port == 0 || port > 65535 {
		return "", fmt.Errorf("%s has no valid HTTP entitlement", app)
	}
	if conn.RegistryDialer == nil {
		host := strings.Trim(conn.Host, "[]")
		if host == "" || strings.HasPrefix(host, "unix:") {
			return "", fmt.Errorf("device has no browser-accessible address")
		}
		return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(int(port))), Path: "/"}).String(), nil
	}
	if forward := b.appForwards[port]; forward != nil {
		return forward.url, nil
	}
	// A loopback listener lets the normal browser use mesh/cloud without a
	// system-wide VPN. Keep the existing authenticated device transport.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	forwardCtx, cancel := context.WithCancel(b.life)
	forward := &appForward{listener: listener, cancel: cancel, url: "http://" + listener.Addr().String() + "/"}
	if b.appForwards == nil {
		b.appForwards = make(map[uint32]*appForward)
	}
	b.appForwards[port] = forward
	dial := conn.RegistryDialer
	go forward.serve(forwardCtx, func(ctx context.Context) (net.Conn, error) { return dial(ctx, int(port)) })
	return forward.url, nil
}

type appForward struct {
	listener net.Listener
	cancel   context.CancelFunc
	url      string
}

func (f *appForward) Close() { f.cancel(); _ = f.listener.Close() }
func (f *appForward) serve(ctx context.Context, dial func(context.Context) (net.Conn, error)) {
	for {
		local, err := f.listener.Accept()
		if err != nil {
			return
		}
		go forwardAppConnection(ctx, local, dial)
	}
}
func forwardAppConnection(ctx context.Context, local net.Conn, dial func(context.Context) (net.Conn, error)) {
	defer local.Close()
	stopLocal := context.AfterFunc(ctx, func() { _ = local.Close() })
	defer stopLocal()
	remote, err := dial(ctx)
	if err != nil {
		return
	}
	defer remote.Close()
	stopRemote := context.AfterFunc(ctx, func() { _ = remote.Close() })
	defer stopRemote()
	done := make(chan struct{}, 2)
	copyStream := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if half, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go copyStream(remote, local)
	go copyStream(local, remote)
	<-done
	select {
	case <-done:
	case <-ctx.Done():
		_ = local.Close()
		_ = remote.Close()
		<-done
	}
}
