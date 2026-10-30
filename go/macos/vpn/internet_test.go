package vpn

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
)

func TestInternetVPNPacketsDNSAndLeaseLoss(t *testing.T) {
	local, donor := net.Pipe()
	defer donor.Close()
	lease := meshsharing.InternetConfig{Address4: "10.88.1.1", Address6: "fd2b:677a:9d11::101", MTU: 1280}
	dnsCalls := make(chan bool, 8)
	uplink := &Internet{Config: lease, Stream: local, DNS: func(context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			wire := &dns.Conn{Conn: server}
			query, err := wire.ReadMsg()
			if err != nil {
				return
			}
			dnsCalls <- true
			reply := new(dns.Msg)
			reply.SetReply(query)
			reply.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: query.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("203.0.113.42")}}
			wire.WriteMsg(reply)
		}()
		return client, nil
	}}
	relay, cfg, err := NewRelayWithInternet(testRoutes(t), nil, nil, uplink)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	engine, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	query := new(dns.Msg)
	query.SetQuestion("example.org.", dns.TypeA)
	wire, _ := query.Pack()
	response, err := engine.answerDNS(wire)
	if err != nil {
		t.Fatal(err)
	}
	var answer dns.Msg
	if err := answer.Unpack(response); err != nil || len(answer.Answer) != 1 {
		t.Fatal("public DNS", answer, err)
	}
	<-dnsCalls
	query.SetQuestion(cfg.Routes[0].Host+".", dns.TypeA)
	wire, _ = query.Pack()
	response, err = engine.answerDNS(wire)
	if err != nil {
		t.Fatal(err)
	}
	answer.Unpack(response)
	if len(answer.Answer) != 1 || answer.Answer[0].(*dns.A).A.String() != cfg.Routes[0].Address {
		t.Fatal("lost device DNS override")
	}
	select {
	case <-dnsCalls:
		t.Fatal("device name leaked to public resolver")
	default:
	}
	for _, version := range []byte{4, 6} {
		packet := make([]byte, 80)
		if version == 4 {
			packet[0] = 0x45
			packet[8] = 64
			packet[9] = 17
			binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
			copy(packet[12:16], net.ParseIP(lease.Address4).To4())
			copy(packet[16:20], net.ParseIP("203.0.113.42").To4())
		} else {
			packet[0] = 0x60
			packet[6] = 17
			packet[7] = 64
			binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)-40))
			copy(packet[8:24], net.ParseIP(lease.Address6).To16())
			copy(packet[24:40], net.ParseIP("2001:db8::42").To16())
		}
		engine.Write(packet)
		donor.SetDeadline(time.Now().Add(time.Second))
		got, err := meshsharing.ReadFrame(donor)
		if err != nil || !bytes.Equal(got, packet) {
			t.Fatal("outgoing packet", err)
		}
		if version == 4 {
			copy(packet[16:20], net.ParseIP(lease.Address4).To4())
			copy(packet[12:16], net.ParseIP("203.0.113.42").To4())
		} else {
			copy(packet[24:40], net.ParseIP(lease.Address6).To16())
			copy(packet[8:24], net.ParseIP("2001:db8::42").To16())
		}
		if err = meshsharing.WriteFrame(donor, packet); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		got = engine.Read(ctx)
		cancel()
		if !bytes.Equal(got, packet) {
			t.Fatal("incoming packet changed")
		}
	}
	donor.Close()
	select {
	case <-engine.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("gateway loss did not stop engine")
	}
	if engine.Healthy() {
		t.Fatal("closed uplink remained healthy")
	}
}
func TestInternetConfigurationAndModernMeshSelectors(t *testing.T) {
	selector := "mesh://11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222"
	routes, err := BuildRoutes([]Route{{Selector: selector}})
	if err != nil {
		t.Fatal(err)
	}
	if routes[0].Host != "device-22222222-2222-4222-8222-222222222222.mesh.wendy.dev" {
		t.Fatal(routes)
	}
	for _, suffix := range []string{"?", "#other", "/x"} {
		if _, err := BuildRoutes([]Route{{Selector: selector + suffix}}); err == nil {
			t.Fatal("accepted malformed selector", suffix)
		}
	}
	cfg := meshsharing.InternetConfig{Address4: "10.88.1.1", MTU: 1280}
	if !cfg.Valid() {
		t.Fatal("valid IPv4-only lease rejected")
	}
	cfg.Address6 = "fd2b:677a:9d11::102"
	if cfg.Valid() {
		t.Fatal("accepted inconsistent lease families")
	}
}
