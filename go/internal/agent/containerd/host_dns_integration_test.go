//go:build linux

package containerd

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// #1833: one running host-network container must follow resolved's new upstream
// after an outage. Neither resolver-file assertions nor host-side DNS queries
// exercise the container's bind-mounted inode and real resolver process.
func TestHostNetworkDNSOutage(t *testing.T) {
	if os.Getenv("WENDY_HOST_DNS_NAMESPACE") != "1" {
		t.Skip("requires test-host-network-dns.sh on a disposable Linux runner")
	}
	for _, ns := range []string{"mnt", "net", "pid"} {
		current, err := os.Readlink("/proc/self/ns/" + ns)
		parent := os.Getenv("WENDY_DNS_PARENT_" + ns)
		if err != nil || parent == "" || current == parent {
			t.Fatalf("%s namespace is not isolated: current=%q parent=%q error=%v", ns, current, parent, err)
		}
	}
	c, ctx := newIntegrationClient(t)
	deadline := time.Now().Add(10 * time.Second)
	for !systemdResolvedStubAvailable() {
		if time.Now().After(deadline) {
			t.Fatal("real systemd-resolved stub did not become available")
		}
		time.Sleep(50 * time.Millisecond)
	}
	stopA := startHostDNSUpstream(t, "192.0.2.53:53", "198.51.100.10")
	startHostDNSUpstream(t, "192.0.2.54:53", "198.51.100.20")
	resolvedCommand := func(args ...string) {
		t.Helper()
		commandCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if output, err := exec.CommandContext(commandCtx, "resolvectl", args...).CombinedOutput(); err != nil {
			t.Fatalf("resolvectl %v: %v\n%s", args, err, output)
		}
	}
	resolvedCommand("dns", "uplink0", "192.0.2.53")
	resolvedCommand("domain", "uplink0", "~regression.test")
	resolvedCommand("default-route", "uplink0", "yes")
	createReplaceTestImage(t, c, ctx)
	const app = "dns-regression"
	cfg := &appconfig.AppConfig{AppID: app, Version: "1", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "host"}}}
	if err := c.CreateContainerWithProgress(ctx, &agentpb.CreateContainerRequest{AppName: app, ImageName: replaceTestImage}, cfg, nil); err != nil {
		t.Fatal(err)
	}
	ctr, err := c.client.LoadContainer(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	task, err := ctr.NewTask(ctx, cio.NullIO)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = task.Delete(cleanupCtx, containerd.WithProcessKill)
		_ = ctr.Delete(cleanupCtx, containerd.WithSnapshotCleanup)
	})
	if err := task.Start(ctx); err != nil {
		t.Fatal(err)
	}
	pid := task.Pid()
	hostNS, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	containerNS, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", pid))
	if err != nil || containerNS != hostNS {
		t.Fatalf("container does not share the isolated host network: %q vs %q, %v", containerNS, hostNS, err)
	}
	lookup := func(name, want string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		fqdn := name + ".regression.test."
		code, err := c.ExecInContainer(queryCtx, app, []string{"/bin/busybox", "timeout", "4", "/bin/busybox", "nslookup", "-type=A", fqdn}, false, nil, &stdout, &stderr, nil)
		if err != nil {
			t.Fatalf("DNS lookup execution failed: %v; stdout=%s stderr=%s", err, &stdout, &stderr)
		}
		if want == "" {
			if code == 0 {
				t.Fatalf("DNS outage unexpectedly resolved: %s", &stdout)
			}
		} else if code != 0 || !strings.Contains(stdout.String(), want) {
			t.Fatalf("container DNS did not recover for %s: exit=%d want=%s stdout=%s stderr=%s", name, code, want, &stdout, &stderr)
		}
		t.Logf("%s lookup: exit=%d, expected address=%q", name, code, want)
		current, err := ctr.Task(ctx, nil)
		if err != nil {
			t.Fatalf("container task disappeared: %v", err)
		}
		status, err := current.Status(ctx)
		if err != nil || status.Status != containerd.Running || current.Pid() != pid {
			t.Fatalf("container restarted or stopped across DNS changes: %+v, %v", status, err)
		}
	}
	lookup("before", "198.51.100.10")
	stopA()
	lookup("during-outage", "")
	resolvedCommand("dns", "uplink0", "192.0.2.54")
	resolvedCommand("flush-caches")
	lookup("after-upstream-replacement", "198.51.100.20")
	// A second container creation atomically replaces the managed source. The
	// first task must still query the stable stub through its original inode.
	if _, err := writeHostResolvConf(); err != nil {
		t.Fatal(err)
	}
	lookup("after-managed-file-replacement", "198.51.100.20")
}

func startHostDNSUpstream(t *testing.T, address, answer string) func() {
	t.Helper()
	conn, err := net.ListenPacket("udp4", address)
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{PacketConn: conn, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		reply := new(dns.Msg)
		reply.SetReply(request)
		reply.Authoritative = true
		for _, question := range request.Question {
			if question.Qtype == dns.TypeA && strings.HasSuffix(question.Name, ".regression.test.") {
				reply.Answer = append(reply.Answer, &dns.A{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0}, A: net.ParseIP(answer)})
			}
		}
		_ = w.WriteMsg(reply)
	})}
	done := make(chan error, 1)
	go func() { done <- server.ActivateAndServe() }()
	stopped := false
	stop := func() {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.ShutdownContext(ctx); err != nil {
			t.Errorf("stop DNS upstream: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("DNS upstream: %v", err)
			}
		case <-ctx.Done():
			t.Error("DNS upstream did not exit")
		}
	}
	t.Cleanup(stop)
	return stop
}
