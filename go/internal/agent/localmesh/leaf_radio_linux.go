//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// ServeLeafRadio accepts a single explicitly configured leaf over an ordered
// radio stream. Physical dial direction is independent of TLS direction: the
// router is the TLS server even when it dials an ESP32's L2CAP listener.
// The caller owns conn. No route is installed until mutual TLS and signed
// manifests pass the same checks as TCP leaves.
func (n *Node) ServeLeafRadio(ctx context.Context, peer meshid.Device, conn net.Conn, timeURL string) error {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := serveLeafTime(ctx, conn, timeURL); err != nil {
		return err
	}
	handshake, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	actual, secure, err := acceptLeafTLS(handshake, n.Credentials, conn)
	if err != nil {
		return err
	}
	if actual != peer {
		return errors.New("radio leaf differs from pinned device")
	}
	return n.AttachLeafStream(ctx, peer, secure)
}

// The request contains DER only, never a URL or credentials. One bounded request
// is relayed to the configured authority per connection. The leaf verifies the
// authority's signature, its random nonce and its persistent time floor itself.
func serveLeafTime(ctx context.Context, conn net.Conn, authority string) error {
	if err := validateLeafTimeURL(authority); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if err := conn.SetDeadline(time.Now().Add(25 * time.Second)); err != nil {
		return err
	}
	defer conn.SetDeadline(time.Time{})
	var header [8]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[4:])
	if string(header[:4]) != "WMT3" || size == 0 || size > 128 {
		return errors.New("invalid leaf timestamp request")
	}
	request := make([]byte, size)
	if _, err := io.ReadFull(conn, request); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authority, bytes.NewReader(request))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/timestamp-query")
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("timestamp redirects are disabled")
	}}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("timestamp authority status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil {
		return err
	}
	if len(body) == 0 || len(body) > 65536 {
		return errors.New("timestamp response size out of range")
	}
	binary.BigEndian.PutUint32(header[:4], uint32(len(body)))
	if _, err := io.Copy(conn, bytes.NewReader(header[:4])); err != nil {
		return err
	}
	_, err = io.Copy(conn, bytes.NewReader(body))
	return err
}
