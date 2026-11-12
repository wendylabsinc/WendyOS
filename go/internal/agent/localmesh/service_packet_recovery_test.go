package localmesh_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh"
)

var errServiceSigningUnavailable = errors.New("fixture manifest signer temporarily unavailable")

type serviceFailureSigner struct {
	crypto.Signer
	armed  atomic.Bool
	failed atomic.Int32
}

func (s *serviceFailureSigner) Sign(random io.Reader, digest []byte, options crypto.SignerOpts) ([]byte, error) {
	if s.armed.Swap(false) {
		s.failed.Add(1)
		return nil, errServiceSigningUnavailable
	}
	return s.Signer.Sign(random, digest, options)
}

func startServicePacketRuntime(t *testing.T, directory string, credentials *localmesh.Credentials) *clockPacketRuntime {
	t.Helper()
	network, err := netmesh.NewNetwork(credentials.Device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	node := localmesh.NewServiceRuntimeNode(t, ctx, directory, credentials, network)
	// Reuse the real packet/TLS lifecycle fixture without its clock seams.
	runtime := &clockPacketRuntime{node: node, network: network, cancel: cancel, done: make(chan error, 1)}
	go func() { runtime.done <- node.Run() }()
	t.Cleanup(func() { runtime.stop(t) })
	return runtime
}

func serviceCatalog(t *testing.T, runtime *clockPacketRuntime) *meshcatalog.Catalog {
	t.Helper()
	catalog, err := meshcatalog.NewManifestCatalog("default", runtime.node.Credentials.Device,
		func(app, kind string, port uint16) error {
			if app != "com.wendy.publication-regression" || kind != "_http._tcp" || (port != 18475 && port != 18476) {
				return errors.New("fixture application entitlement denied")
			}
			return nil
		}, runtime.node.ControlHub().ReplaceServices, runtime.node.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func serviceIdentityEvidence(t *testing.T, old, renewed, receiver *localmesh.Credentials) {
	t.Helper()
	before, err := x509.ParseCertificate(old.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	after, err := x509.ParseCertificate(renewed.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.RawSubjectPublicKeyInfo, after.RawSubjectPublicKeyInfo) || !after.NotAfter.After(before.NotAfter) {
		t.Fatal("fixture renewal did not preserve the key and extend leaf validity")
	}
	report := map[string]any{"publisher": old.Device.Principal(), "receiver": receiver.Device.Principal(),
		"old_sha256": fmt.Sprintf("%x", sha256.Sum256(before.Raw)), "renewed_sha256": fmt.Sprintf("%x", sha256.Sum256(after.Raw)),
		"old_not_after": before.NotAfter, "renewed_not_after": after.NotAfter,
		"stale_descriptor_direction": "receiver retains and describes publisher's old leaf to renewed publisher"}
	if directory := os.Getenv("WENDY_SERVICE_PACKET_EVIDENCE"); directory != "" {
		path, err := os.MkdirTemp(directory, "service-identities-")
		if err != nil {
			t.Fatal(err)
		}
		for name, credentials := range map[string]*localmesh.Credentials{"old": old, "renewed": renewed, "receiver": receiver} {
			var chain []byte
			for _, der := range credentials.Certificate.Certificate {
				chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
			}
			if err := os.WriteFile(filepath.Join(path, name+"-public-chain.pem"), chain, 0600); err != nil {
				t.Fatal(err)
			}
		}
		data, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(filepath.Join(path, "identity-directions.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		t.Log("public identity evidence:", path)
	}
	t.Logf("service certificate renewal: %+v", report)
}

// Actual TCP packets, TLS, complete binary manifests and durable runtime
// restart; no host TUN or radio. Only the signing failure and an ordinary Babel
// sequence-request stimulus are supplied by the test. Production timers remain.
func TestServicePublishFailureRecoversTrafficWithRenewedIdentity(t *testing.T) {
	if os.Getenv("WENDY_E2E_SERVICE_PACKET_RECOVERY") != "1" {
		t.Skip("run through test-service-packet-recovery.sh in the existing network job")
	}
	credentials, receiverCredentials := localmesh.ServiceRuntimeCredentials(t)
	directory := filepath.Join(t.TempDir(), "publisher")
	a := startServicePacketRuntime(t, directory, credentials)
	b := startServicePacketRuntime(t, filepath.Join(t.TempDir(), "receiver"), receiverCredentials)
	remote := serviceCatalog(t, b)
	accepted := meshcatalog.PublishSpec{AppID: "com.wendy.publication-regression", ServiceID: "temperature", Type: "_http._tcp", Instance: "Publication fixture", HostPort: 18475}
	publish := func(runtime *clockPacketRuntime) *meshcatalog.Catalog {
		t.Helper()
		catalog := serviceCatalog(t, runtime)
		if _, err := catalog.Publish(accepted, time.Now()); err != nil {
			t.Fatal(err)
		}
		return catalog
	}
	publish(a)
	closeEcho := clockEchoServer(t, a.network)
	closeLink := connectClockPacketRuntimes(t, a, b)
	waitAccepted := func(after uint64, leaf []byte) meshcatalog.Record {
		t.Helper()
		var result meshcatalog.Record
		clockWait(t, func() bool {
			rows := remote.Snapshot(time.Now())
			if len(rows) != 1 || rows[0].Key.Device != credentials.Device.Principal() || rows[0].Key.ServiceID != accepted.ServiceID || rows[0].Generation <= after || !clockHasPeerRoute(b, a) {
				return false
			}
			if !bytes.Equal(localmesh.ServiceRuntimeAcceptedLeaf(b.node, credentials.Device.Principal()), leaf) {
				return false
			}
			result = rows[0]
			return true
		}, "live receiver did not accept the service and exact renewed signing leaf")
		return result
	}
	dialAdvertised := func(row meshcatalog.Record) net.Conn {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		conn, err := b.network.DialContext(ctx, net.JoinHostPort(credentials.Device.MeshAddress().String(), fmt.Sprint(row.HostPort)))
		if err != nil {
			t.Fatalf("accepted service became unreachable after failed publication: advertised port=%d: %v", row.HostPort, err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	echo := func(conn net.Conn, phase string) {
		t.Helper()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		payload := []byte("service-packet-" + phase)
		if _, err := conn.Write(payload); err != nil {
			t.Fatalf("accepted service TCP failed at %s: %v", phase, err)
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("accepted service TCP failed at %s: %v", phase, err)
		}
	}
	first := waitAccepted(0, credentials.Certificate.Certificate[0])
	oldStream := dialAdvertised(first)
	echo(oldStream, "original-leaf")
	_ = oldStream.Close()
	closeEcho()
	closeLink()
	a.stop(t)
	previousRevision := localmesh.ClockRuntimeRevision(t, a.node)
	renewed := localmesh.RenewServiceRuntimeCredentials(t, credentials)
	if renewed.Device != credentials.Device || bytes.Equal(renewed.Certificate.Certificate[0], credentials.Certificate.Certificate[0]) {
		t.Fatal("fixture did not renew the same device identity")
	}
	serviceIdentityEvidence(t, credentials, renewed, receiverCredentials)
	signer := &serviceFailureSigner{Signer: renewed.Signer}
	renewed.Signer = signer // Assigned before the runtime starts; TLS key stays real.
	a = startServicePacketRuntime(t, directory, renewed)
	catalog := publish(a) // The running app advertises its accepted service again.
	closeEcho = clockEchoServer(t, a.network)
	closeLink = connectClockPacketRuntimes(t, a, b)
	current := waitAccepted(previousRevision, renewed.Certificate.Certificate[0])
	stream := dialAdvertised(current)
	echo(stream, "renewed-leaf-before-failure")
	t.Log("renewed TLS identity and complete manifest reached the original receiver; advertised service TCP works")
	// Let the real one-second publication gate open; a queued edit would be
	// accepted and could not establish this synchronous rejection contract.
	time.Sleep(1100 * time.Millisecond)
	signer.armed.Store(true)
	rejected := accepted
	rejected.HostPort = 18476
	if _, err := catalog.Publish(rejected, time.Now()); !errors.Is(err, errServiceSigningUnavailable) || signer.failed.Load() != 1 {
		t.Fatalf("replacement did not encounter the real manifest signer failure: %v failures=%d", err, signer.failed.Load())
	}
	for _, projection := range []*meshcatalog.Catalog{catalog, remote} {
		rows := projection.Snapshot(time.Now())
		if len(rows) != 1 || rows[0].HostPort != accepted.HostPort || rows[0].Generation != current.Generation {
			t.Fatalf("failed edit changed the accepted service projection: %+v", rows)
		}
	}
	echo(stream, "while-replacement-rejected")
	// Trigger ordinary re-signing without replacing service intent. Removing
	// Node's failure rollback must leak the rejected port into this new proof.
	localmesh.RequestServiceRuntimeRenewal(t, a.node)
	recovered := waitAccepted(current.Generation, renewed.Certificate.Certificate[0])
	// Dial what the independent receiver actually discovered, before inspecting
	// the expected port. A poisoned service projection must fail real traffic.
	fresh := dialAdvertised(recovered)
	echo(fresh, "discovered-after-recovery")
	echo(stream, "same-stream-after-recovery")
	if recovered.HostPort != accepted.HostPort || signer.failed.Load() != 1 {
		t.Fatal("ordinary renewal did not retain the accepted service")
	}
	_ = fresh.Close()
	_ = stream.Close()
	closeEcho()
	closeLink()
	a.stop(t)
	b.stop(t)
	t.Log("accepted advertised service and established TCP survived signer recovery")
}
