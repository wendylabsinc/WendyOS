//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	quic "github.com/quic-go/quic-go"
)

type nodeTestConfig struct {
	Asset                                 int32
	Cert, Chain, Key, Dir, Underlay, Peer string
	Internet                              bool
}

func TestNodePeerProcess(t *testing.T) {
	path := os.Getenv("WENDY_LOCALMESH_NODE_TEST_CONFIG")
	if path == "" {
		t.Skip("isolated test subprocess")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config nodeTestConfig
	if err = json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	credentials, err := NewCredentials(64, config.Asset, config.Cert, config.Chain, config.Key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	node, err := NewNode(ctx, config.Dir, credentials, "test", 50052)
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- node.Run() }()
	defer func() { cancel(); <-runDone }()
	peer := int32(445)
	if config.Asset == 445 {
		peer = 460
	}
	tlsConfig, err := credentials.PeerTLS(peer)
	if err != nil {
		t.Fatal(err)
	}
	if config.Asset == 460 {
		if config.Internet {
			policy, err := NewHostPolicy(ctx, config.Asset)
			if err != nil {
				t.Fatal(err)
			}
			defer policy.Close()
			if err = policy.SetSharing(ctx, "wlmu1", "10.201.0.1"); err != nil {
				t.Fatal(err)
			}
			if err = node.SetUplink(ctx, "wlmu1"); err != nil {
				t.Fatal(err)
			}
		}
		server := &http.Server{Addr: "10.88.1.204:18080", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.RemoteAddr, "10.88.1.189:") {
				http.Error(w, "request bypassed mesh source addressing", http.StatusForbidden)
				return
			}
			fmt.Fprint(w, "hello over Babel and QUIC")
		})}
		defer server.Close()
		go server.ListenAndServe()
		listener, err := quic.ListenAddr(config.Underlay, tlsConfig, QUICConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		conn, err := listener.Accept(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_ = node.Attach(ctx, peer, conn)
		return
	}
	var conn *quic.Conn
	for ctx.Err() == nil {
		attempt, cancelAttempt := context.WithTimeout(ctx, time.Second)
		conn, err = quic.DialAddr(attempt, config.Peer, tlsConfig, QUICConfig())
		cancelAttempt()
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	linkDone := make(chan error, 1)
	go func() { linkDone <- node.Attach(ctx, peer, conn) }()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	for ctx.Err() == nil {
		response, err := client.Get("http://10.88.1.204:18080/")
		if err == nil {
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err == nil && string(body) == "hello over Babel and QUIC" {
				found := false
				for _, m := range node.Snapshot().Devices {
					if m.Asset == 460 {
						found = true
					}
				}
				if found {
					if config.Internet {
						if err = node.SetRoaming(ctx, true); err != nil {
							t.Fatal(err)
						}
						query := new(dns.Msg)
						query.SetQuestion("internet.test.", dns.TypeA)
						answer, _, err := (&dns.Client{Timeout: time.Second}).Exchange(query, "10.88.1.204:53")
						if err != nil || len(answer.Answer) == 0 {
							continue
						}
						response, err := client.Get("http://10.201.0.1:18081/")
						if err != nil {
							continue
						}
						body, err := io.ReadAll(response.Body)
						response.Body.Close()
						if err != nil || string(body) != "shared uplink" {
							continue
						}
						t.Log("Internet default routing, source NAT, and peer DNS forwarding succeeded")
					}
					t.Log("HTTP and signed device discovery succeeded across live Babel/QUIC/TUN nodes")
					cancel()
					<-linkDone
					return
				}
			}
		}
		select {
		case err := <-linkDone:
			t.Fatal("link ended", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("timed out", node.Snapshot())
}

func TestNodePairIsolatedNamespaces(t *testing.T) {
	testNodePair(t, false)
}

func TestInternetSharingIsolatedNamespaces(t *testing.T) { testNodePair(t, true) }

func testNodePair(t *testing.T, sharing bool) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container")
	}
	if os.Getenv("WENDY_LOCALMESH_NODE_TEST_CONFIG") != "" {
		t.Skip("parent test only")
	}
	names := []string{fmt.Sprintf("wlma%d", os.Getpid()), fmt.Sprintf("wlmb%d", os.Getpid())}
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %v: %v %s", args, err, out)
		}
	}
	for _, name := range names {
		run("netns", "add", name)
		defer exec.Command("ip", "netns", "del", name).Run()
	}
	run("link", "add", "wlmtesta", "type", "veth", "peer", "name", "wlmtestb")
	run("link", "set", "wlmtesta", "netns", names[0])
	run("link", "set", "wlmtestb", "netns", names[1])
	for i, name := range names {
		iface := []string{"wlmtesta", "wlmtestb"}[i]
		run("-n", name, "addr", "add", fmt.Sprintf("10.200.0.%d/30", i+1), "dev", iface)
		run("-n", name, "link", "set", iface, "up")
		run("-n", name, "link", "set", "lo", "up")
	}
	if sharing {
		run("link", "add", "wlmu0", "type", "veth", "peer", "name", "wlmu1")
		defer exec.Command("ip", "link", "del", "wlmu0").Run()
		run("link", "set", "wlmu1", "netns", names[1])
		run("addr", "add", "10.201.0.1/30", "dev", "wlmu0")
		run("link", "set", "wlmu0", "up")
		run("-n", names[1], "addr", "add", "10.201.0.2/30", "dev", "wlmu1")
		run("-n", names[1], "link", "set", "wlmu1", "up")
		run("-n", names[1], "route", "add", "default", "via", "10.201.0.1")
		listener, err := net.Listen("tcp4", "10.201.0.1:18081")
		if err != nil {
			t.Fatal(err)
		}
		httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.RemoteAddr, "10.201.0.2:") {
				http.Error(w, "missing source NAT", 403)
				return
			}
			fmt.Fprint(w, "shared uplink")
		})}
		defer httpServer.Close()
		go httpServer.Serve(listener)
		socket, err := net.ListenPacket("udp4", "10.201.0.1:53")
		if err != nil {
			t.Fatal(err)
		}
		dnsServer := &dns.Server{PacketConn: socket, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
			m := new(dns.Msg)
			m.SetReply(r)
			if len(r.Question) > 0 {
				m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: net.ParseIP("10.201.0.1")}}
			}
			_ = w.WriteMsg(m)
		})}
		defer dnsServer.Shutdown()
		go dnsServer.ActivateAndServe()
	}
	dir := t.TempDir()
	configs := map[int32]string{}
	testCredentialsWithPEM(t, func(asset int32, cert, chain, key string) {
		cfg := nodeTestConfig{Asset: asset, Cert: cert, Chain: chain, Key: key, Dir: filepath.Join(dir, fmt.Sprint(asset)), Underlay: "10.200.0.2:18500", Peer: "10.200.0.2:18500", Internet: sharing}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, fmt.Sprint(asset)+".json")
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		configs[asset] = path
	})
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	server := exec.CommandContext(ctx, "ip", "netns", "exec", names[1], binary, "-test.run", "^TestNodePeerProcess$", "-test.v")
	server.Env = append(os.Environ(), "WENDY_LOCALMESH_NODE_TEST_CONFIG="+configs[460])
	var serverOutput bytes.Buffer
	server.Stdout = &serverOutput
	server.Stderr = &serverOutput
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	client := exec.CommandContext(ctx, "ip", "netns", "exec", names[0], binary, "-test.run", "^TestNodePeerProcess$", "-test.v")
	client.Env = append(os.Environ(), "WENDY_LOCALMESH_NODE_TEST_CONFIG="+configs[445])
	out, clientErr := client.CombinedOutput()
	serverErr := server.Wait()
	if clientErr != nil || serverErr != nil {
		t.Fatalf("client: %v\n%s\nserver: %v\n%s", clientErr, out, serverErr, serverOutput.String())
	}
	if !strings.Contains(string(out), "HTTP and signed device discovery succeeded") {
		t.Fatal(string(out))
	}
	t.Log(string(out))
}
