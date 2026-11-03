package tsarelay

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"time"
)

// BLETimePSM carries one end-to-end HTTPS timestamp exchange before the
// requesting device has this boot's signed clock. The native mesh stays on129.
const BLETimePSM uint16 = 130

// TunnelConnection relays ciphertext only to the fixed timestamp authority.
// The peer supplies neither a destination nor any credentials to this relay;
// HTTPS hostname/CA and the signed nonce-bound proof remain peer-verified.
// The connection is consumed and the complete exchange is bounded to30seconds.
func TunnelConnection(parent context.Context, incoming net.Conn) error {
	authority, err := url.Parse(Authority)
	if err != nil {
		_ = incoming.Close()
		return err
	}
	return tunnelConnection(parent, incoming, net.JoinHostPort(authority.Hostname(), "443"))
}

func tunnelConnection(parent context.Context, incoming net.Conn, destination string) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	defer incoming.Close()
	stopIncoming := context.AfterFunc(ctx, func() { _ = incoming.Close() })
	defer stopIncoming()
	outgoing, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", destination)
	if err != nil {
		return fmt.Errorf("timestamp HTTPS relay dial: %w", err)
	}
	defer outgoing.Close()
	stopOutgoing := context.AfterFunc(ctx, func() { _ = outgoing.Close() })
	defer stopOutgoing()
	deadline, _ := ctx.Deadline()
	_ = incoming.SetDeadline(deadline)
	_ = outgoing.SetDeadline(deadline)
	done := make(chan error, 2)
	copyBytes := func(to, from net.Conn) {
		// Hide optional ReaderFrom/WriterTo methods so buffers stay bounded.
		_, err := io.CopyBuffer(struct{ io.Writer }{to}, struct{ io.Reader }{from}, make([]byte, 1024))
		if closer, ok := to.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		done <- err
	}
	go copyBytes(outgoing, incoming)
	go copyBytes(incoming, outgoing)
	first := <-done
	if first != nil {
		cancel()
	}
	second := <-done
	if first != nil {
		return first
	}
	return second
}
