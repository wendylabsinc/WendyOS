//go:build linux

package meshsharing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestUplinkFamilyProbeProcess(t *testing.T) {
	phase := os.Getenv("WENDY_UPLINK_PROBE_PHASE")
	if phase == "" {
		t.Skip("namespace subprocess")
	}
	ctx := context.Background()
	if got := (linuxProbe{}).Uplink(ctx, "uplink0", "10.202.0.1"); got != (phase == "dual") {
		t.Fatalf("IPv4 uplink health=%v in %s phase; IPv6 HTTPS must not establish IPv4 health", got, phase)
	}
	if !probeUplinkIPv6(ctx, "uplink0", "10.202.0.1") {
		t.Fatal("independent IPv6 positive control failed")
	}
	if (linuxProbe{}).Uplink(ctx, "missing0", "10.202.0.1") || (linuxProbe{}).Uplink(ctx, "wlmp1", "10.202.0.1") {
		t.Fatal("probe escaped its bound physical interface or accepted a mesh interface")
	}
}

// Exercise the production DNS resolver, SO_BINDTODEVICE sockets, HTTPS trust
// and family selection. IPv4 DNS works throughout, but initially only the
// IPv6 TLS endpoint listens. The old unconstrained dial falsely declared the
// mandatory IPv4 uplink healthy by reaching that IPv6 endpoint.
func TestUplinkFamilyProbeAcrossNamespaces(t *testing.T) {
	if os.Getenv("WENDY_MESH_NAMESPACE_TEST") != "1" {
		t.Skip("requires a disposable privileged Linux container")
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %v: %s", args, err, out)
		}
	}
	ns, host := fmt.Sprintf("wmpf%d", os.Getpid()), fmt.Sprintf("wpf%d", os.Getpid())
	run("netns", "add", ns)
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })
	run("-n", ns, "link", "set", "lo", "up")
	run("link", "add", host, "type", "veth", "peer", "name", "uplink0", "netns", ns)
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", host).Run() })
	run("addr", "add", "10.202.0.1/30", "dev", host)
	run("-6", "addr", "add", "2001:db8:202::1/64", "dev", host, "nodad")
	run("link", "set", host, "up")
	run("-n", ns, "addr", "add", "10.202.0.2/30", "dev", "uplink0")
	run("-n", ns, "-6", "addr", "add", "2001:db8:202::2/64", "dev", "uplink0", "nodad")
	run("-n", ns, "link", "set", "uplink0", "up")
	run("-n", ns, "route", "add", "default", "via", "10.202.0.1")
	run("-n", ns, "-6", "route", "add", "default", "via", "2001:db8:202::1")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "probe fixture"},
		DNSNames: []string{"connectivitycheck.gstatic.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "root.pem")
	if err := os.WriteFile(root, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	var queries, requests4, requests6 atomic.Int32
	packet, err := net.ListenPacket("udp4", "10.202.0.1:53")
	if err != nil {
		t.Fatal(err)
	}
	dnsServer := &dns.Server{PacketConn: packet, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		queries.Add(1)
		m := new(dns.Msg)
		m.SetReply(r)
		for _, question := range r.Question {
			if question.Name != "connectivitycheck.gstatic.com." {
				m.Rcode = dns.RcodeNameError
				continue
			}
			hdr := dns.RR_Header{Name: question.Name, Rrtype: question.Qtype, Class: dns.ClassINET, Ttl: 1}
			switch question.Qtype {
			case dns.TypeA:
				m.Answer = append(m.Answer, &dns.A{Hdr: hdr, A: net.ParseIP("10.202.0.1")})
			case dns.TypeAAAA:
				m.Answer = append(m.Answer, &dns.AAAA{Hdr: hdr, AAAA: net.ParseIP("2001:db8:202::1")})
			}
		}
		_ = w.WriteMsg(m)
	})}
	go dnsServer.ActivateAndServe()
	t.Cleanup(func() { _ = dnsServer.Shutdown(); _ = packet.Close() })
	serve := func(network, address string, count *atomic.Int32) {
		t.Helper()
		listener, err := net.Listen(network, address)
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count.Add(1)
			if r.URL.Path != "/generate_204" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})}
		secure := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS13})
		go server.Serve(secure)
		t.Cleanup(func() { _ = server.Close() })
	}
	serve("tcp6", "[2001:db8:202::1]:443", &requests6)
	probe := func(phase string) {
		t.Helper()
		cmd := exec.Command("ip", "netns", "exec", ns, os.Args[0], "-test.run=^TestUplinkFamilyProbeProcess$", "-test.v")
		cmd.Env = append(os.Environ(), "WENDY_UPLINK_PROBE_PHASE="+phase, "SSL_CERT_FILE="+root, "SSL_CERT_DIR="+filepath.Dir(root),
			"HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1", "NO_PROXY=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s production probe: %v\n%s", phase, err, out)
		}
		t.Logf("%s: %s", phase, out)
	}
	probe("ipv6-only")
	if requests4.Load() != 0 || requests6.Load() == 0 || queries.Load() == 0 {
		t.Fatal("missing independent DNS/IPv6 witnesses")
	}
	serve("tcp4", "10.202.0.1:443", &requests4)
	probe("dual")
	if requests4.Load() == 0 {
		t.Fatal("IPv4 healthy result lacked actual IPv4 HTTPS traffic")
	}
}
