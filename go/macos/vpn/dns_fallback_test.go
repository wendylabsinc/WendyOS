package vpn

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

func TestPlatformDNSFallback(t *testing.T) {
	for _, tt := range []struct {
		name     string
		class    uint16
		fallback bool
		fail     bool
		code     int
	}{
		{"example.org", dns.ClassINET, true, false, dns.RcodeSuccess},
		{"example.org", dns.ClassINET, true, true, dns.RcodeServerFailure},
		{"example.org", dns.ClassCHAOS, false, false, dns.RcodeRefused},
		{"device-missing.mesh.wendy.dev", dns.ClassINET, false, false, dns.RcodeNameError},
		{"device-missing.cloud.wendy.dev", dns.ClassINET, false, false, dns.RcodeNameError},
		{"nested.private.mesh.wendy.dev", dns.ClassINET, false, false, dns.RcodeRefused},
	} {
		t.Run(tt.name+dns.ClassToString[tt.class]+string(rune('0'+tt.code)), func(t *testing.T) {
			called := false
			cfg := Config{Endpoint: "127.0.0.1:8080", Token: strings.Repeat("a", 64), Routes: testRoutes(t)}
			engine, err := NewEngineWithDNSFallback(cfg, func(ctx context.Context, wire []byte) ([]byte, error) {
				called = true
				if _, ok := ctx.Deadline(); !ok {
					t.Error("resolver has no deadline")
				}
				if tt.fail {
					return nil, errors.New("offline")
				}
				var query dns.Msg
				if err := query.Unpack(wire); err != nil {
					return nil, err
				}
				return new(dns.Msg).SetReply(&query).Pack()
			})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			q := new(dns.Msg).SetQuestion(dns.Fqdn(tt.name), dns.TypeA)
			q.Question[0].Qclass = tt.class
			wire, _ := q.Pack()
			answer, err := engine.answerDNS(wire)
			if err != nil {
				t.Fatal(err)
			}
			var reply dns.Msg
			if err := reply.Unpack(answer); err != nil {
				t.Fatal(err)
			}
			if called != tt.fallback || reply.Rcode != tt.code || reply.Id != q.Id {
				t.Fatalf("fallback=%v reply=%+v", called, reply)
			}
			// A selected private device must resolve locally even with a fallback.
			called = false
			q.SetQuestion(dns.Fqdn(cfg.Routes[0].Host), dns.TypeA)
			wire, _ = q.Pack()
			answer, err = engine.answerDNS(wire)
			if err != nil {
				t.Fatal(err)
			}
			if err = reply.Unpack(answer); err != nil {
				t.Fatal(err)
			}
			if called || len(reply.Answer) != 1 {
				t.Fatal("private DNS escaped to platform resolver")
			}
		})
	}
}
