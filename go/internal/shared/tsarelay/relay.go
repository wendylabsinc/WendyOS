// Package tsarelay relays bounded nonce-bearing RFC3161 requests to a fixed
// public authority. Clients must verify the enrolled TSA, nonce and clock floor.
package tsarelay

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

const Authority = "https://timestamp.sigstore.dev/api/v1/timestamp"
const Port = 43025

func newClient() *http.Client {
	return &http.Client{Timeout: 20 * time.Second,
		Transport: &http.Transport{Proxy: nil, TLSHandshakeTimeout: 5 * time.Second,
			MaxConnsPerHost: 4, MaxIdleConns: 4, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// ServeConnection handles one already device-authenticated app TLS stream.
// The caller retains its certificate deadline and cancellation; the request
// selects no destination, only a nonce-bearing query to the fixed public TSA.
func ServeConnection(parent context.Context, conn net.Conn) error {
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	client := newClient()
	defer client.CloseIdleConnections()
	return forward(ctx, conn, client, Authority)
}

// Query is the same fixed-authority operation for an authenticated router
// control stream. It opens no additional device session or inbound listener.
func Query(ctx context.Context, body []byte) ([]byte, error) {
	if len(body) == 0 || len(body) > 128 {
		return nil, fmt.Errorf("invalid timestamp request length")
	}
	client := newClient()
	defer client.CloseIdleConnections()
	return query(ctx, client, Authority, body)
}
func query(ctx context.Context, client *http.Client, url string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/timestamp-query")
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("timestamp authority HTTP %d", response.StatusCode)
	}
	reply, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(reply) == 0 || len(reply) > 65536 {
		return nil, fmt.Errorf("invalid timestamp response size: %d (%v)", len(reply), err)
	}
	return reply, nil
}

// Serve owns a carrier-bound listener and closes all active requests when the
// carrier stops. The request contains no URL, credentials or arbitrary payload
// destination; only the fixed public TSA can be contacted.
func Serve(parent context.Context, listener net.Listener) error {
	ctx, cancel := context.WithCancel(parent)
	client := newClient()
	var workers sync.WaitGroup
	defer func() { cancel(); _ = listener.Close(); workers.Wait(); client.CloseIdleConnections() }()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	active := make(chan struct{}, 4)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case active <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-active }()
			request, cancelRequest := context.WithTimeout(ctx, 25*time.Second)
			defer cancelRequest()
			closeOnCancel := context.AfterFunc(request, func() { _ = conn.Close() })
			defer closeOnCancel()
			_ = forward(request, conn, client, Authority)
		}()
	}
}

func forward(ctx context.Context, c net.Conn, client *http.Client, url string) error {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	var header [8]byte
	if _, err := io.ReadFull(c, header[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(header[4:])
	if string(header[:4]) != "WNT1" || n == 0 || n > 128 {
		return fmt.Errorf("invalid signed-time request frame")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c, body); err != nil {
		return err
	}
	reply, err := query(ctx, client, url, body)
	if err != nil {
		return err
	}
	binary.BigEndian.PutUint32(header[:4], uint32(len(reply)))
	if _, err = io.Copy(c, io.MultiReader(bytes.NewReader(header[:4]), bytes.NewReader(reply))); err != nil {
		return err
	}
	return nil
}
