//go:build ios

package useripc

import (
	"context"
	"net"
)

// App Group paths exceed Darwin's sockaddr_un limit. The file instead carries
// an ephemeral loopback endpoint and mutual TLS identity shared by the app and
// its extension. Other apps cannot read this private App Group file.
func Listen(path string) (net.Listener, error)                { return listenLoopback(path) }
func Dial(ctx context.Context, path string) (net.Conn, error) { return dialLoopback(ctx, path) }
