//go:build !windows

package sessionbroker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh/carriers"
	"github.com/wendylabsinc/wendy/go/internal/cli/operatormesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Only discovery is supplied by the fixture. The real broker owns startup,
// diagnostics, TLS admission, carrier scheduling and the production packet
// network. This does not exercise native radio discovery or a graphical client.
func TestOperatorColdFailureRecoversWithSpareCarrierTraffic(t *testing.T) {
	if os.Getenv("WENDY_E2E_OPERATOR_SPARE") != "1" {
		t.Skip("run through test-operator-spare-recovery.sh in the existing network job")
	}
	operatorBrokerNormal(t, "P256-spare-recovery", false)
}

type operatorSpareFixture struct {
	router   meshid.Device
	bad      net.Listener
	good     []net.Listener
	phase    atomic.Int32
	failed   atomic.Int32
	sessions chan *operatormesh.Session
	specs    chan Spec
	fast     chan net.Conn
}

func newOperatorSpareFixture(t *testing.T, router meshid.Device, good []net.Listener) *operatorSpareFixture {
	t.Helper()
	bad, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &operatorSpareFixture{router: router, bad: bad, good: good, sessions: make(chan *operatormesh.Session, 8), specs: make(chan Spec, 8), fast: make(chan net.Conn, 8)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := bad.Accept()
			if err != nil {
				return
			}
			// Read the real TLS record header before closing, so the client has
			// attempted a handshake and receives EOF/reset, not a fake dial error.
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			var header [5]byte
			if _, err := io.ReadFull(conn, header[:]); err == nil && header[0] == 22 {
				f.failed.Add(1)
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = bad.Close(); <-done })
	return f
}

func (f *operatorSpareFixture) source(ctx context.Context, emit func([]carriers.Candidate)) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		candidate := func(listener net.Listener, cost uint16) carriers.Candidate {
			return carriers.Candidate{Device: f.router, Transport: carriers.TCP, Address: listener.Addr().String(), Cost: cost}
		}
		items := []carriers.Candidate{candidate(f.bad, 512)}
		switch f.phase.Load() {
		case 1, 3:
			items = []carriers.Candidate{candidate(f.good[0], 512)}
		case 2:
			items = []carriers.Candidate{candidate(f.good[0], 512), candidate(f.good[1], 64)}
		}
		emit(items)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (f *operatorSpareFixture) accepted(listener net.Listener, conn net.Conn) uint16 {
	if listener == f.good[1] {
		f.fast <- conn
		return 64
	}
	return 512
}

func spareWait(t *testing.T, ctx context.Context, label string, check func() bool) {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	for !check() {
		select {
		case <-wait.Done():
			t.Fatalf("spare-carrier journey: %s: %v", label, wait.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (f *operatorSpareFixture) run(t *testing.T, ctx context.Context, certificate config.CertificateInfo, network *netmesh.Network, service *operatorBrokerAgent, admitted, withdrawn *atomic.Int32) {
	t.Helper()
	cold, cancel := context.WithTimeout(ctx, 4*time.Second)
	ready, err := EnableOperatorAutomatic(cold, certificate)
	cancel()
	if ready != nil {
		_ = ready.Close()
	}
	var failure *operatorStartupFailure
	if !errors.As(err, &failure) || failure.failure.Kind != "connection_closed" || failure.failure.Transport != carriers.TCP || f.failed.Load() == 0 || admitted.Load() != 0 || service.calls.Load() != 0 {
		t.Fatalf("cold real TLS failure lost bounded broker diagnostic: %v attempts=%d admissions=%d", err, f.failed.Load(), admitted.Load())
	}
	var spec Spec
	select {
	case spec = <-f.specs:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	directory, err := brokerDir()
	if err != nil {
		t.Fatal(err)
	}
	path := operatorFailurePath(directory, spec)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt operatorFailure
	if json.Unmarshal(data, &receipt) != nil || receipt.Generation != spec.DiagnosticGeneration || receipt.Fingerprint != spec.CertFingerprint || receipt.Kind != "connection_closed" || len(data) > operatorFailureBytes || info.Mode().Perm() != 0600 {
		t.Fatalf("cold diagnostic is not private, bounded and generation-bound: %s", data)
	}
	for _, private := range []string{f.bad.Addr().String(), certificate.PemPrivateKey, certificate.PrincipalURI} {
		if private != "" && strings.Contains(string(data), private) {
			t.Fatal("cold diagnostic persisted a private connection detail")
		}
	}
	if ok, advice := OperatorRecovery(failure); !ok || len(advice) == 0 {
		t.Fatal("cold failure has no actionable recovery")
	}
	// End the failed broker using the production stop path. Recovery must open
	// a fresh owner, not reuse the joining Session retained by this test.
	if err := DisconnectOperator(ctx); err != nil {
		t.Fatal(err)
	}
	var coldSession *operatormesh.Session
	select {
	case coldSession = <-f.sessions:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	f.phase.Store(1)
	ready, err = EnableOperatorAutomatic(ctx, certificate)
	if err != nil {
		t.Fatal("healthy carrier did not recover cold broker", err)
	}
	defer ready.Close()
	var session *operatormesh.Session
	select {
	case session = <-f.sessions:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if session == coldSession || session.Node.Credentials.Device != coldSession.Node.Credentials.Device || session.Info().Revision <= coldSession.Info().Revision {
		t.Fatal("cold recovery did not retain the endpoint with a fresh durable revision")
	}
	var recovered Spec
	select {
	case recovered = <-f.specs:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if recovered.DiagnosticGeneration == spec.DiagnosticGeneration || recovered.CertFingerprint != spec.CertFingerprint {
		t.Fatal("cold recovery did not create a fresh diagnostic generation with retained credentials")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("healthy new owner retained stale cold failure", err)
	}
	checkRPC := func() {
		t.Helper()
		request, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		client, err := ConnectOperator(request, MeshSelector(f.router))
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		before := service.calls.Load()
		if _, err := client.AgentService.GetAgentVersion(request, &agentpb.GetAgentVersionRequest{}); err != nil || service.calls.Load() <= before {
			t.Fatal("recovered authenticated management RPC", err)
		}
	}
	checkRPC()
	listener, err := network.ListenTCP(29476)
	if err != nil {
		t.Fatal(err)
	}
	echoDone := make(chan struct{})
	echoAccepted := make(chan net.Conn, 1)
	var echoed atomic.Uint64
	go func() {
		defer close(echoDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		echoAccepted <- conn
		_ = conn.SetDeadline(time.Now().Add(45 * time.Second))
		var buffer [4096]byte
		for {
			n, err := conn.Read(buffer[:])
			if err != nil {
				return
			}
			if _, err := conn.Write(buffer[:n]); err != nil {
				return
			}
			echoed.Add(uint64(n))
		}
	}()
	var stream net.Conn
	defer func() {
		if stream != nil {
			_ = stream.Close()
		}
		_ = listener.Close()
		select {
		case accepted := <-echoAccepted:
			_ = accepted.Close()
		case <-echoDone:
		case <-time.After(3 * time.Second):
			t.Error("packet echo accept did not drain")
		}
		select {
		case <-echoDone:
		case <-time.After(3 * time.Second):
			t.Error("packet echo worker did not drain")
		}
	}()
	stream, err = session.DialContext(ctx, "tcp", net.JoinHostPort(f.router.MeshAddress().String(), "29476"))
	if err != nil {
		t.Fatal(err)
	}
	echo := func(phase string) {
		t.Helper()
		payload := strings.Repeat(phase+"|", 137)
		_ = stream.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.WriteString(stream, payload); err != nil {
			t.Fatalf("persistent packet stream failed at %s: %v", phase, err)
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(stream, got); err != nil || string(got) != payload {
			t.Fatalf("persistent packet stream failed at %s: %v", phase, err)
		}
	}
	selected := func(cost uint16, count int) bool {
		view := session.Snapshot()
		if len(view.Links) != count {
			return false
		}
		for _, link := range view.Links {
			if link.Device == f.router && link.Cost == cost && link.ForwardingReady && !link.Retiring {
				return true
			}
		}
		return false
	}
	spareWait(t, ctx, "initial slow carrier forwarding", func() bool { return selected(512, 1) })
	echo("cold-recovered-slow")
	t.Log("real slow packet stream and authenticated RPC recovered the cold broker")
	f.phase.Store(2)
	spareWait(t, ctx, "faster carrier selected with slow standby retained", func() bool { return selected(64, 2) })
	if admitted.Load() != 2 || withdrawn.Load() != 0 {
		t.Fatal("spare admission replaced a live incumbent", admitted.Load(), withdrawn.Load())
	}
	// Hold the spare across several production scheduler polls, with real
	// packets each time. A transient two-link snapshot is not retention.
	for range 4 {
		echo("fast-with-slow-standby")
		time.Sleep(300 * time.Millisecond)
		if !selected(64, 2) || withdrawn.Load() != 0 {
			t.Fatal("standby was retired despite spare capacity and live fast traffic")
		}
	}
	checkRPC()
	// Withdraw fast discovery and close only the fixture's exact fast socket.
	// The original slow incarnation must carry the same application TCP stream.
	f.phase.Store(3)
	_ = f.good[1].Close()
	select {
	case fast := <-f.fast:
		_ = fast.Close()
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	spareWait(t, ctx, "original standby selected after fast carrier loss", func() bool { return selected(512, 1) && withdrawn.Load() == 1 })
	echo("original-slow-after-fast-loss")
	checkRPC()
	if admitted.Load() != 2 || echoed.Load() == 0 {
		t.Fatal("fallback redialed the incumbent instead of retaining the standby")
	}
	if err := DisconnectOperator(ctx); err != nil {
		t.Fatal(err)
	}
	spareWait(t, ctx, "all healthy carrier owners drained", func() bool { return withdrawn.Load() == 2 })
	t.Log("cold diagnostic and recovery passed; original TCP stream survived faster admission and standby fallback")
}
