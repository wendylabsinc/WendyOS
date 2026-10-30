package vpn

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
)

// Internet is a single leased packet stream owned by the app's relay. DNS uses
// the gateway's mesh resolver, never the host resolver captured by the VPN.
type Internet struct {
	Config  meshsharing.InternetConfig
	Stream  net.Conn
	DNS     func(context.Context) (net.Conn, error)
	claimed atomic.Bool
	closed  atomic.Bool
	once    sync.Once
}

func (u *Internet) Closed() bool { return u.closed.Load() }

func (u *Internet) Close() { u.once.Do(func() { u.closed.Store(true); u.Stream.Close() }) }

func (e *Engine) answerDNS(wire []byte) ([]byte, error) {
	answer, err := AnswerDNS(wire, e.cfg.Routes)
	if err != nil || e.cfg.Internet == nil {
		return answer, err
	}
	var local dns.Msg
	if err = local.Unpack(answer); err != nil {
		return nil, err
	}
	if local.Rcode != dns.RcodeRefused {
		return answer, nil
	}
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	conn, err := requestRelay(ctx, e.cfg, flowRequest{Token: e.cfg.Token, Kind: "internet-dns"}, 10*time.Second)
	if err != nil {
		local.Rcode = dns.RcodeServerFailure
		return local.Pack()
	}
	defer conn.Close()
	var query dns.Msg
	if err = query.Unpack(wire); err != nil {
		return nil, err
	}
	response, _, err := (&dns.Client{Net: "tcp", Timeout: 10 * time.Second}).ExchangeWithConnContext(ctx, &query, &dns.Conn{Conn: conn})
	if err != nil {
		local.Rcode = dns.RcodeServerFailure
		return local.Pack()
	}
	return response.Pack()
}

func (e *Engine) startInternet() error {
	conn, err := requestRelay(e.ctx, e.cfg, flowRequest{Token: e.cfg.Token, Kind: "internet"}, 10*time.Second)
	if err != nil {
		return err
	}
	e.internet = conn
	e.pumpWG.Add(2)
	go func() {
		defer e.pumpWG.Done()
		defer e.cancel()
		for {
			packet, err := meshsharing.ReadFrame(conn)
			if err != nil {
				return
			}
			_, dest, ok := meshsharing.PacketAddresses(packet)
			if !ok || (dest.String() != e.cfg.Internet.Address4 && dest.String() != e.cfg.Internet.Address6) {
				continue
			}
			select {
			case e.packets <- packet:
			case <-e.ctx.Done():
				return
			}
		}
	}()
	go func() {
		defer e.pumpWG.Done()
		defer e.cancel()
		for {
			select {
			case <-e.ctx.Done():
				return
			case p := <-e.internetWrites:
				if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
					return
				}
				if meshsharing.WriteFrame(conn, p) != nil {
					return
				}
			}
		}
	}()
	return nil
}

// Internet packets retain their original IP and transport headers. Device VIPs
// and the local resolver still use the split-tunnel stack.
func (e *Engine) writeInternet(packet []byte) bool {
	if e.cfg.Internet == nil {
		return false
	}
	source, dest, ok := meshsharing.PacketAddresses(packet)
	if !ok {
		return true
	}
	if dest.String() == DNSAddress || e.allowed(dest.String()) {
		return false
	}
	if devicePrefix.Contains(dest) {
		return true
	}
	if source.String() != e.cfg.Internet.Address4 && source.String() != e.cfg.Internet.Address6 {
		return true
	}
	if len(packet) > e.cfg.Internet.MTU {
		return true
	}
	select {
	case <-e.ctx.Done():
	case e.internetWrites <- append([]byte(nil), packet...):
	default:
	}
	return true
}

// ReadInternetConfig consumes the initial bounded metadata frame before the
// extension installs any default routes.
func ReadInternetConfig(conn net.Conn) (meshsharing.InternetConfig, error) {
	var cfg meshsharing.InternetConfig
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return cfg, err
	}
	n := binary.BigEndian.Uint16(size[:])
	if n == 0 || n > 1024 {
		return cfg, errors.New("invalid internet configuration size")
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(conn, data); err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	if !cfg.Valid() {
		return cfg, errors.New("invalid gateway address lease")
	}
	return cfg, nil
}
