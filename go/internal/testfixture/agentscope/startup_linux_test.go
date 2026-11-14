//go:build linux

package agentscope

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// This runs the actual Agent executable, never main() inside the test process.
// Its global filesystem/service startup belongs only in the disposable container.
func TestAgentOwnScopeStartup(t *testing.T) {
	if os.Getenv("WENDY_AGENT_SCOPE_FIXTURE") == "" {
		t.Skip("requires the disposable startup container")
	}
	admission, err := os.ReadFile("/scope-admission")
	if err != nil || string(admission) != os.Getenv("WENDY_AGENT_SCOPE_FIXTURE") || len(admission) != 32 || os.Getpid() != 1 {
		t.Fatal("startup fixture requires its admitted container PID namespace")
	}
	if _, err := os.Stat("/.dockerenv"); err != nil {
		t.Fatal("requires disposable Docker filesystem")
	}
	// No startup probe may use a destination in the kernel's ephemeral source
	// range: before bind, a TCP connect can select that same port and self-connect.
	ephemeral, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		t.Fatal(err)
	}
	if err := admitScopePorts(string(ephemeral), scopeAgentPort); err != nil {
		t.Fatal(err)
	}
	t.Logf("startup ports admitted: setup=%d mTLS=%d ephemeral=%s", scopeAgentPort, scopeAgentPort+1, bytes.TrimSpace(ephemeral))
	scenario := os.Getenv("WENDY_SCOPE_CASE")
	mode, identity := "strict", "unknown"
	switch scenario {
	case "unknown-strict":
	case "unknown-grace":
		mode = "grace"
	case "unknown-off":
		mode = "off"
	case "legacy-strict":
		identity = "legacy"
	case "spiffe-strict":
		identity = "spiffe"
	default:
		t.Fatalf("unknown case %q", scenario)
	}
	refused := identity == "unknown" && mode != "off"
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rootTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "private startup CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	raw, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw})
	issue := func(serial int64, uri string) ([]byte, []byte, tls.Certificate) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "scope-fixture"}, DNSNames: []string{"localhost"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		if uri != "" {
			parsed, err := url.Parse(uri)
			if err != nil {
				t.Fatal(err)
			}
			leaf.URIs = []*url.URL{parsed}
		}
		raw, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, rootKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certPEM, keyPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return certPEM, keyPEM, pair
	}
	ownURI, clientURI := "", "urn:wendy:org:7:user:5"
	if identity == "legacy" {
		ownURI = "urn:wendy:org:7:asset:42"
	}
	if identity == "spiffe" {
		ownURI = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/box-42"
		clientURI = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice"
	}
	certPEM, keyPEM, ownPair := issue(2, ownURI)
	_, _, clientPair := issue(3, clientURI)
	config := "/etc/wendy-agent"
	if err := os.Mkdir(config, 0700); err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(map[string]any{"enrolled": true, "orgId": 7, "assetId": 42, "certPem": string(certPEM), "chainPem": string(rootPEM)})
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"provisioning.json": state, "device-key.pem": keyPEM} {
		if err := os.WriteFile(filepath.Join(config, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	log, err := os.Create("/tmp/scope-agent.log")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	command := exec.Command("/scope-agent")
	command.Env = append(os.Environ(), "WENDY_CONFIG_PATH="+config, "WENDY_AGENT_PORT="+strconv.Itoa(scopeAgentPort), "WENDY_LOCAL_SOCKET=off", "WENDY_CONTAINERD_ADDR=/tmp/absent-containerd.sock", "WENDY_MTLS_ORG_ENFORCEMENT="+mode)
	command.Stdout, command.Stderr = log, log
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	exited := false
	t.Cleanup(func() {
		if !exited {
			_ = command.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
				exited = true
			case <-time.After(5 * time.Second):
				_ = command.Process.Kill()
				select {
				case <-done:
					exited = true
				case <-time.After(5 * time.Second):
					t.Error("Agent process did not drain")
				}
			}
		}
		if data, err := os.ReadFile("/tmp/scope-agent.log"); err == nil {
			t.Logf("actual Agent output:\n%s", data)
		}
	})
	roots := x509.NewCertPool()
	roots.AddCert(root)
	rpc := func(address string) error {
		conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "localhost", Certificates: []tls.Certificate{clientPair}})))
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var remote peer.Peer
		reply, err := agentpb.NewWendyAgentServiceClient(conn).GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}, grpc.Peer(&remote))
		if err != nil {
			return err
		}
		info, ok := remote.AuthInfo.(credentials.TLSInfo)
		if !ok || len(info.State.PeerCertificates) == 0 || !bytes.Equal(info.State.PeerCertificates[0].Raw, ownPair.Certificate[0]) || reply.GetVersion() == "" {
			return fmt.Errorf("wrong actual Agent RPC identity/result")
		}
		return nil
	}
	deadline := time.Now().Add(75 * time.Second)
	refusalSeen := time.Time{}
	var lastRPCError error
	for {
		select {
		case err := <-done:
			exited = true
			t.Fatalf("actual Agent exited before assertion: %v", err)
		default:
		}
		data, err := os.ReadFile("/tmp/scope-agent.log")
		if err != nil {
			t.Fatal(err)
		}
		for _, failure := range []string{"Failed to listen on mTLS port", "Failed to create mTLS server", "Failed to parse agent port for mTLS"} {
			if bytes.Contains(data, []byte(`"msg":"`+failure+`"`)) {
				t.Fatalf("actual startup failed: %s; full Agent output follows in cleanup", failure)
			}
		}
		refusedLog := bytes.Contains(data, []byte("cannot determine device identity scope; refusing to start mTLS server"))
		if refusedLog && refusalSeen.IsZero() {
			refusalSeen = time.Now()
		}
		// Wait for the actual listener or refusal before any TCP probe. This
		// also avoids creating client sockets while the Agent is still binding.
		probeAllowed := refusedLog || bytes.Contains(data, []byte(`"msg":"mTLS gRPC server listening"`))
		address := ""
		for _, candidate := range []string{fmt.Sprintf("127.0.0.1:%d", scopeAgentPort+1), fmt.Sprintf("[::1]:%d", scopeAgentPort+1)} {
			if !probeAllowed {
				break
			}
			if connection, err := net.DialTimeout("tcp", candidate, 100*time.Millisecond); err == nil {
				connection.Close()
				address = candidate
				break
			}
		}
		if address != "" {
			if err := rpc(address); err == nil {
				if refused {
					t.Fatal("UNKNOWN_SCOPE_STARTUP_EXPOSED actual mTLS Agent RPC succeeded")
				}
				break
			} else {
				lastRPCError = err
				if refused {
					t.Fatalf("unknown-scope Agent exposed a listener: %v", err)
				}
			}
		}
		if refused && !refusalSeen.IsZero() && time.Since(refusalSeen) >= 3*time.Second {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("actual startup boundary absent: case=%s refusal=%v probeAllowed=%v lastRPCError=%v", scenario, refusedLog, probeAllowed, lastRPCError)
		}
		time.Sleep(30 * time.Millisecond)
	}
	// The refusal must not silently reopen plaintext provisioning as recovery.
	for _, address := range []string{fmt.Sprintf("127.0.0.1:%d", scopeAgentPort), fmt.Sprintf("[::1]:%d", scopeAgentPort)} {
		if connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
			connection.Close()
			t.Fatal("enrolled Agent reopened plaintext setup")
		}
	}
	for name, expected := range map[string][]byte{"provisioning.json": state, "device-key.pem": keyPEM} {
		actual, err := os.ReadFile(filepath.Join(config, name))
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("startup changed private enrollment %s: %v", name, err)
		}
	}
	t.Logf("AGENT_OWN_SCOPE_VERIFIED case=%s refused=%v own_leaf_sha256=%x", scenario, refused, sha256.Sum256(ownPair.Certificate[0]))
}
