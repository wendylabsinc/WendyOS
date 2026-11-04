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

// ProbeFunc succeeds only after the selected device's agent responds.
type ProbeFunc func(context.Context, string) error

const echoTimeout = 5 * time.Second

// BLE lease setup can take several seconds per candidate. Keep control RPCs
// bounded while allowing two candidate handshakes and directory convergence.
const RefreshTimeout = 40 * time.Second

type Relay struct {
	listener  net.Listener
	cancel    context.CancelFunc
	done      chan struct{}
	wg        sync.WaitGroup
	internet  *Internet
	refreshMu sync.RWMutex
	refresh   func(context.Context, bool) (Config, error)
}
type flowRequest struct {
	Token   string `json:"token"`
	Address string `json:"address"`
	Port    uint16 `json:"port"`
	Kind    string `json:"kind,omitempty"`
	Online  bool   `json:"online,omitempty"`
}

func NewRelay(routes []Route, dial DialFunc, probe ProbeFunc) (*Relay, Config, error) {
	return NewRelayWithInternet(routes, dial, probe, nil)
}

func NewRelayWithInternet(routes []Route, dial DialFunc, probe ProbeFunc, internet *Internet) (*Relay, Config, error) {
	var err error
	if len(routes) > 0 {
		routes, err = BuildRoutes(routes)
	}
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
	if routes == nil {
		routes = []Route{}
	}
	cfg := Config{Endpoint: ln.Addr().String(), Token: hex.EncodeToString(token), Routes: routes}
	if internet != nil {
		if internet.Stream == nil || internet.DNS == nil || !internet.Config.Valid() {
			ln.Close()
			return nil, Config{}, errors.New("invalid internet relay")
		}
		cfg.Internet = &internet.Config
	}
	ctx, cancel := context.WithCancel(context.Background())
	relay := &Relay{listener: ln, cancel: cancel, done: make(chan struct{}), internet: internet}
	byIP := map[string]Route{}
	for _, r := range routes {
		byIP[r.Address] = r
	}
	go func() {
		defer close(relay.done)
		slots := make(chan struct{}, 128)
		echoSlots := make(chan struct{}, 8)
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
				if req.Kind == "refresh" {
					relay.refreshMu.RLock()
					refresh := relay.refresh
					relay.refreshMu.RUnlock()
					if refresh == nil {
						return
					}
					update, cancelUpdate := context.WithTimeout(ctx, RefreshTimeout)
					defer cancelUpdate()
					conn.SetDeadline(time.Now().Add(22 * time.Second))
					next, err := refresh(update, req.Online)
					if err != nil {
						return
					}
					if _, err = io.WriteString(conn, "OK\n"); err == nil {
						_ = json.NewEncoder(conn).Encode(next)
					}
					return
				}
				if req.Kind == "" && req.Port == 0 && req.Address == "ping" {
					if internet == nil || !internet.closed.Load() {
						io.WriteString(conn, "PONG\n")
					}
					return
				}
				if req.Kind == "internet" || req.Kind == "internet-dns" {
					if internet == nil || internet.closed.Load() || req.Address != "" || req.Port != 0 {
						return
					}
					var upstream net.Conn
					if req.Kind == "internet" {
						if !internet.claimed.CompareAndSwap(false, true) {
							return
						}
						upstream = internet.Stream
						defer internet.Close()
					} else {
						dnsCtx, cancelDNS := context.WithTimeout(ctx, 10*time.Second)
						upstream, err = internet.DNS(dnsCtx)
						cancelDNS()
						if err == nil {
							upstream.SetDeadline(time.Now().Add(10 * time.Second))
						}
						if err != nil {
							return
						}
					}
					defer upstream.Close()
					stop := context.AfterFunc(ctx, func() { upstream.Close() })
					defer stop()
					conn.SetDeadline(time.Time{})
					if _, err = io.WriteString(conn, "OK\n"); err != nil {
						return
					}
					Pipe(&bufferedConn{Conn: conn, reader: reader}, upstream)
					return
				}
				if req.Kind != "" && req.Kind != "echo" {
					return
				}
				route, ok := byIP[req.Address]
				if !ok {
					return
				}
				if req.Kind == "echo" {
					if req.Port != 0 || probe == nil {
						return
					}
					select {
					case echoSlots <- struct{}{}:
					default:
						return
					}
					defer func() { <-echoSlots }()
					probeCtx, cancelProbe := context.WithTimeout(ctx, echoTimeout)
					defer cancelProbe()
					if err := probe(probeCtx, route.Selector); err != nil || probeCtx.Err() != nil {
						io.WriteString(conn, "ERR\n")
						return
					}
					io.WriteString(conn, "OK\n")
					return
				}
				if req.Port == 0 {
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
func (r *Relay) Close() {
	r.cancel()
	r.listener.Close()
	if r.internet != nil {
		r.internet.Close()
	}
	<-r.done
	r.wg.Wait()
}
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
	return requestRelay(ctx, c, flowRequest{Token: c.Token, Address: address, Port: port}, 65*time.Second)
}

// ProbeRelay checks a device through the authenticated relay. The loopback
// heartbeat cannot stand in for a device response.
func ProbeRelay(ctx context.Context, c Config, address string) error {
	conn, err := requestRelay(ctx, c, flowRequest{Token: c.Token, Address: address, Kind: "echo"}, echoTimeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

func requestRelay(ctx context.Context, c Config, req flowRequest, timeout time.Duration) (net.Conn, error) {
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
	conn.SetDeadline(time.Now().Add(timeout))
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err = json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(conn, 4096)
	line, err := readLine(reader)
	if err != nil {
		return nil, err
	}
	if string(line) != "OK" {
		if req.Kind == "echo" {
			return nil, errors.New("device did not answer VPN probe")
		}
		return nil, errors.New("device tunnel refused port " + strconv.Itoa(int(req.Port)))
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

// SetRefresh installs the authenticated automatic routing control operation.
func (r *Relay) SetRefresh(refresh func(context.Context, bool) (Config, error)) {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	r.refresh = refresh
}

func Refresh(ctx context.Context, controller Config, online bool) (Config, error) {
	conn, err := requestRelay(ctx, controller, flowRequest{Token: controller.Token, Kind: "refresh", Online: online}, RefreshTimeout+5*time.Second)
	if err != nil {
		return Config{}, err
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(RefreshTimeout + 5*time.Second))
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	var cfg Config
	err = json.NewDecoder(io.LimitReader(conn, 1024*1024)).Decode(&cfg)
	if err == nil {
		err = cfg.Validate()
	}
	return cfg, err
}
