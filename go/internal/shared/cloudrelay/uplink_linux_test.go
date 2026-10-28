//go:build linux

package cloudrelay

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestUplinkDNSAddresses(t *testing.T) {
	got := parseUplinkDNS("127.0.0.53\n::1\n0.0.0.0\n224.0.0.1\n--\n192.0.2.1\n192.0.2.1\nfe80::1%old\n2001:db8::53", "wifi0")
	if !slices.Equal(got, []string{"192.0.2.1", "fe80::1%wifi0", "2001:db8::53"}) {
		t.Fatalf("DNS servers: %v", got)
	}
}

func TestCloudUplinkPreservesExplicitProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	conn, err := DialBroker("broker.example:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.Target() != "broker.example:443" {
		t.Fatal("custom dialer bypassed explicit proxy")
	}
}

func TestCloudUplinkNamespaceProcess(t *testing.T) {
	endpoint := os.Getenv("WENDY_UPLINK_ENDPOINT")
	if endpoint == "" {
		t.Skip("namespace subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := DialBroker(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: grantKeyTransport, Timeout: 10 * time.Second}
	resp, err := client.Get(os.Getenv("WENDY_UPLINK_KEYS_URL"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != "grant keys" {
		t.Fatalf("grant keys: %q, %v", body, err)
	}
	a := &Agent{Verifier: &Verifier{}}
	if a.presencePathReachable(ctx, endpoint) {
		t.Fatal("primary-only probe succeeded through the fallback; it would flap working presence")
	}
}

// Run in a disposable Linux container with NET_ADMIN and SYS_ADMIN. Both interfaces have
// default routes, but the preferred Ethernet gateway and its DNS never answer.
// Only sockets bound to Wi-Fi can reach the test Cloud and its DNS server.
func TestCloudUplinkAcrossNamespaces(t *testing.T) {
	if os.Getenv("WENDY_CLOUD_UPLINK_NAMESPACE_TEST") != "1" {
		t.Skip("requires disposable Linux container with NET_ADMIN and SYS_ADMIN")
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %v: %s", args, err, out)
		}
	}
	ns, host := fmt.Sprintf("wcu%d", os.Getpid()), fmt.Sprintf("wcu%d", os.Getpid())
	run("netns", "add", ns)
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })
	run("-n", ns, "link", "set", "lo", "up")
	run("link", "add", host, "type", "veth", "peer", "name", "wifi0", "netns", ns)
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", host).Run() })
	run("addr", "add", "198.18.222.1/30", "dev", host)
	run("link", "set", host, "up")
	run("addr", "add", "203.0.113.7/32", "dev", "lo")
	t.Cleanup(func() { _ = exec.Command("ip", "addr", "del", "203.0.113.7/32", "dev", "lo").Run() })
	run("-n", ns, "addr", "add", "198.18.222.2/30", "dev", "wifi0")
	run("-n", ns, "link", "set", "wifi0", "up")
	run("-n", ns, "route", "add", "default", "via", "198.18.222.1", "metric", "100")
	run("-n", ns, "link", "add", "eth0", "type", "dummy")
	run("-n", ns, "link", "set", "eth0", "up")
	run("-n", ns, "addr", "add", "192.0.2.2/24", "dev", "eth0")
	run("-n", ns, "route", "add", "default", "via", "192.0.2.1", "metric", "10")
	resolvDir := filepath.Join("/etc/netns", ns)
	if err := os.MkdirAll(resolvDir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(resolvDir) })
	if err := os.WriteFile(filepath.Join(resolvDir, "resolv.conf"), []byte("nameserver 192.0.2.1\noptions timeout:1 attempts:1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fixture := t.TempDir()
	// Supply the per-interface DNS data normally obtained from NetworkManager.
	if err := os.WriteFile(filepath.Join(fixture, "nmcli"), []byte("#!/bin/sh\ncase \"$*\" in *wifi0) echo 198.18.222.1;; esac\n"), 0755); err != nil {
		t.Fatal(err)
	}
	packet, err := net.ListenPacket("udp4", "198.18.222.1:53")
	if err != nil {
		t.Fatal(err)
	}
	dnsServer := &dns.Server{PacketConn: packet, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(request)
		for _, q := range request.Question {
			if q.Name == "example.com." && q.Qtype == dns.TypeA {
				response.Answer = append(response.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1}, A: net.ParseIP("203.0.113.7")})
			}
		}
		_ = w.WriteMsg(response)
	})}
	go dnsServer.ActivateAndServe()
	t.Cleanup(func() { dnsServer.Shutdown(); packet.Close() })
	keys := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "grant keys") }))
	keys.Listener.Close()
	keys.Listener, err = net.Listen("tcp4", "203.0.113.7:0")
	if err != nil {
		t.Fatal(err)
	}
	keys.StartTLS()
	t.Cleanup(keys.Close)
	root := filepath.Join(fixture, "ca.pem")
	if err := os.WriteFile(root, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: keys.Certificate().Raw}), 0644); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "203.0.113.7:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: keys.TLS.Certificates, MinVersion: tls.VersionTLS13})))
	healthpb.RegisterHealthServer(server, health.NewServer())
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	_, keysPort, _ := net.SplitHostPort(keys.Listener.Addr().String())
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, dnsAddress := range []string{"192.0.2.1", "198.18.222.1"} {
		t.Run("system DNS "+dnsAddress, func(t *testing.T) {
			// Exercise both DNS failure and a resolved Cloud IP whose preferred
			// route blackholes TCP. Ethernet stays preferred in both cases.
			if err := os.WriteFile(filepath.Join(resolvDir, "resolv.conf"), []byte("nameserver "+dnsAddress+"\noptions timeout:1 attempts:1\n"), 0644); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "ip", "netns", "exec", ns, binary, "-test.run=^TestCloudUplinkNamespaceProcess$", "-test.v")
			cmd.Env = append(os.Environ(), "PATH="+fixture+":"+os.Getenv("PATH"), "SSL_CERT_FILE="+root, "SSL_CERT_DIR="+fixture,
				"WENDY_UPLINK_ENDPOINT=example.com:"+port, "WENDY_UPLINK_KEYS_URL=https://example.com:"+keysPort)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("namespace client: %v\n%s", err, out)
			}
		})
	}
}
