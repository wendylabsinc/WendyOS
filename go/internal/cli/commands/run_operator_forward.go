package commands

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// A browser cannot use the CLI's userspace IP stack. Each attached app viewer
// owns a bounded loopback forward through the same authenticated app stream.
// Closing the run context or selected connection joins its listener and flows.
var operatorBrowserSlots = make(chan struct{}, 8)

type operatorBrowserForward struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (f *operatorBrowserForward) Close() error { f.once.Do(f.cancel); <-f.done; return nil }

func prepareOperatorBrowserURL(ctx context.Context, conn *grpcclient.AgentConnection, cfg *appconfig.AppConfig, host string) (string, error) {
	if conn == nil || !conn.OperatorMeshTarget.IsValid() || conn.AppDialer == nil || cfg == nil || cfg.Hooks == nil || cfg.Hooks.PostStart == nil || cfg.Hooks.PostStart.OpenURL == "" {
		return "", nil
	}
	expanded := expandHookEnv(cfg.Hooks.PostStart.OpenURL, urlSafeHost(host), cfg.AppID, cfg.ServiceName)
	address, err := url.Parse(expanded)
	if err != nil {
		return "", err
	}
	// External documentation or other explicit URLs retain the ordinary hook
	// path. Only this selected device's derived address becomes an app forward.
	target := net.ParseIP(conn.OperatorMeshTarget.MeshAddress().String())
	if ip := net.ParseIP(address.Hostname()); ip == nil || !ip.Equal(target) {
		return "", nil
	}
	if address.Scheme != "http" && address.Scheme != "https" {
		return "", fmt.Errorf("mesh browser URL must use http or https")
	}
	portText := address.Port()
	if portText == "" {
		switch address.Scheme {
		case "http":
			portText = "80"
		case "https":
			portText = "443"
		default:
			return "", fmt.Errorf("mesh browser URL must use http or https")
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("mesh browser URL has no usable TCP port")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	select {
	case operatorBrowserSlots <- struct{}{}:
	default:
		return "", fmt.Errorf("all %d mesh app viewers are active", cap(operatorBrowserSlots))
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		<-operatorBrowserSlots
		return "", err
	}
	owner, cancel := context.WithCancel(ctx)
	forward := &operatorBrowserForward{cancel: cancel, done: make(chan struct{})}
	stopParent := context.AfterFunc(ctx, func() { _ = forward.Close() })
	go func() {
		defer close(forward.done)
		defer listener.Close()
		defer func() { <-operatorBrowserSlots }()
		defer cancel()
		defer stopParent()
		_ = serveMeshForward(owner, listener, func(ctx context.Context) (net.Conn, error) { return conn.AppDialer(ctx, uint16(port)) })
	}()
	if !conn.AddCloser(forward) {
		return "", fmt.Errorf("selected mesh connection has closed")
	}
	address.Host = listener.Addr().String()
	return address.String(), nil
}
