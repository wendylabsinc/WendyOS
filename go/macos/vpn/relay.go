package vpn

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

type DialFunc func(context.Context, string, uint16) (net.Conn, error)
type Relay struct {
	listener net.Listener
	cancel   context.CancelFunc
	done     chan struct{}
	wg       sync.WaitGroup
}
type flowRequest struct {
	Token   string `json:"token"`
	Address string `json:"address"`
	Port    uint16 `json:"port"`
}

func NewRelay(routes []Route, dial DialFunc) (*Relay, Config, error) {
	routes, err := BuildRoutes(routes)
	if err != nil {
		return nil, Config{}, err
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, Config{}, err
	}
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		ln.Close()
		return nil, Config{}, err
	}
	cfg := Config{Endpoint: ln.Addr().String(), Token: hex.EncodeToString(token), Routes: routes}
	ctx, cancel := context.WithCancel(context.Background())
	relay := &Relay{listener: ln, cancel: cancel, done: make(chan struct{})}
	byIP := map[string]Route{}
	for _, r := range routes {
		byIP[r.Address] = r
	}
	go func() {
		defer close(relay.done)
		slots := make(chan struct{}, 128)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				conn.Close()
				continue
			}
			relay.wg.Add(1)
			go func() {
				defer relay.wg.Done()
				defer func() { <-slots }()
				defer conn.Close()
				after := context.AfterFunc(ctx, func() { conn.Close() })
				defer after()
				conn.SetDeadline(time.Now().Add(10 * time.Second))
				reader := bufio.NewReaderSize(conn, 4096)
				line, err := readLine(reader)
				if err != nil {
					return
				}
				var req flowRequest
				if json.Unmarshal(line, &req) != nil || subtle.ConstantTimeCompare([]byte(req.Token), []byte(cfg.Token)) != 1 {
					return
				}
				if req.Port == 0 && req.Address == "ping" {
					io.WriteString(conn, "PONG\n")
					return
				}
				if req.Port == 0 {
					return
				}
				route, ok := byIP[req.Address]
				if !ok {
					return
				}
				conn.SetDeadline(time.Time{})
				flowCtx, cancelFlow := context.WithCancel(ctx)
				defer cancelFlow()
				dialTimeout := time.AfterFunc(60*time.Second, cancelFlow)
				upstream, err := dial(flowCtx, route.Selector, req.Port)
				dialTimeout.Stop()
				if err != nil {
					io.WriteString(conn, "ERR\n")
					return
				}
				defer upstream.Close()
				stop := context.AfterFunc(ctx, func() { upstream.Close() })
				defer stop()
				if _, err = io.WriteString(conn, "OK\n"); err != nil {
					return
				}
				Pipe(&bufferedConn{Conn: conn, reader: reader}, upstream)
			}()
		}
	}()
	return relay, cfg, nil
}
func (r *Relay) Close() { r.cancel(); r.listener.Close(); <-r.done; r.wg.Wait() }
func readLine(r *bufio.Reader) ([]byte, error) {
	line, prefix, err := r.ReadLine()
	if prefix {
		return nil, errors.New("relay header too long")
	}
	return line, err
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *bufferedConn) CloseWrite() error {
	if w, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return w.CloseWrite()
	}
	return nil
}
func DialRelay(ctx context.Context, c Config, address string, port uint16) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", c.Endpoint)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()
	conn.SetDeadline(time.Now().Add(65 * time.Second))
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err = json.NewEncoder(conn).Encode(flowRequest{c.Token, address, port}); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(conn, 4096)
	line, err := readLine(reader)
	if err != nil {
		return nil, err
	}
	if string(line) != "OK" {
		return nil, errors.New("device tunnel refused port " + strconv.Itoa(int(port)))
	}
	conn.SetDeadline(time.Time{})
	success = true
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

// Pipe preserves TCP half-close so request bodies followed by EOF still receive
// their response. A full close or cancellation is owned by the caller.
func Pipe(a, b net.Conn) {
	done := make(chan struct{})
	go func() {
		_, err := io.Copy(a, b)
		if err != nil {
			a.Close()
			b.Close()
		} else {
			halfClose(a)
		}
		close(done)
	}()
	_, err := io.Copy(b, a)
	if err != nil {
		a.Close()
		b.Close()
	} else {
		halfClose(b)
	}
	<-done
}
func halfClose(c net.Conn) {
	if w, ok := c.(interface{ CloseWrite() error }); ok {
		_ = w.CloseWrite()
	} else {
		_ = c.Close()
	}
}

func Healthy(ctx context.Context, c Config) bool {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", c.Endpoint)
	if err != nil {
		return false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if json.NewEncoder(conn).Encode(flowRequest{Token: c.Token, Address: "ping"}) != nil {
		return false
	}
	line, err := readLine(bufio.NewReaderSize(conn, 4096))
	return err == nil && string(line) == "PONG"
}
