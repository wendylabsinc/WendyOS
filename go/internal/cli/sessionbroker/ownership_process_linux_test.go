//go:build linux && regression_chunk_ownership

package sessionbroker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type ownershipAgent struct {
	agentpb.UnimplementedWendyAgentServiceServer
	calls atomic.Int64
}

func (a *ownershipAgent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	a.calls.Add(1)
	return &agentpb.GetAgentVersionResponse{Version: "ownership-wire-fixture"}, nil
}

func ownershipPKI(t *testing.T) (config.CertificateInfo, tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ephemeral ownership fixture"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, root, root, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	issue := func(principal string, usage x509.ExtKeyUsage) (string, string) {
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, err := url.Parse(principal)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), URIs: []*url.URL{uri}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		cert, err := x509.CreateCertificate(rand.Reader, leaf, root, leafKey.Public(), key)
		if err != nil {
			t.Fatal(err)
		}
		private, err := x509.MarshalECPrivateKey(leafKey)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}))
	}
	operator, private := issue("urn:wendy:org:7:user:ownership-fixture", x509.ExtKeyUsageClientAuth)
	server, serverKey := issue("urn:wendy:org:7:asset:42", x509.ExtKeyUsageServerAuth)
	pair, err := tls.X509KeyPair([]byte(server), []byte(serverKey))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	return config.CertificateInfo{OrganizationID: 7, PemCertificate: operator, PemPrivateKey: private, PemCertificateChain: chain}, pair, pool
}

func TestRootBrokerLeavesUserStateForRealUnprivilegedRPC(t *testing.T) {
	ownershipBrokerProcess(t, true)
}

func TestBrokerUnprivilegedRoundTripControl(t *testing.T) {
	ownershipBrokerProcess(t, false)
}

func ownershipBrokerProcess(t *testing.T, elevated bool) {
	t.Helper()
	uid, gid := os.Getuid(), os.Getgid()
	if elevated {
		if os.Getenv("WENDY_CHUNK_OWNERSHIP") != "1" {
			t.Skip("hosted ownership gate only")
		}
		if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" || !filepath.IsAbs(os.Getenv("WENDY_OWNER_SCRATCH")) {
			t.Fatal("requires disposable hosted scratch")
		}
		var e1, e2 error
		uid, e1 = strconv.Atoi(os.Getenv("WENDY_OWNER_UID"))
		gid, e2 = strconv.Atoi(os.Getenv("WENDY_OWNER_GID"))
		if os.Getuid() != 0 || e1 != nil || e2 != nil || uid <= 0 || gid <= 0 {
			t.Fatal("requires root and an explicit unprivileged owner")
		}
	} else if uid == 0 {
		t.Skip("positive control requires an ordinary user")
	}
	base := os.Getenv("WENDY_OWNER_SCRATCH")
	if base == "" {
		base = "/tmp"
	}
	root, err := os.MkdirTemp(base, "wbo-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err = os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "user")
	if err = os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if elevated {
		if err = os.Chown(directory, uid, gid); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("WENDY_CONFIG_DIR", directory)
	t.Setenv("HOME", root)
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("WENDY_SECRET_STORE", "file")
	t.Setenv("WENDY_TLS_SESSION_STORE", "off")
	certificate, pair, pool := ownershipPKI(t)
	cfg := &config.Config{CurrentContext: "ownership", Auth: []config.AuthConfig{{Name: "ownership", CloudDashboard: "https://fixture.invalid", CloudGRPC: "fixture.invalid:443", Certificates: []config.CertificateInfo{certificate}}}}
	if err = config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if elevated {
		if err = os.Chown(filepath.Join(directory, "config.json"), uid, gid); err != nil {
			t.Fatal(err)
		}
	}
	agent := &ownershipAgent{}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert})))
	agentpb.RegisterWendyAgentServiceServer(server, agent)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() {
		server.Stop()
		listener.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("owned TLS server did not drain")
		}
	}()
	expected := certs.WendyIdentity{OrgID: 7, EntityType: "asset", EntityID: "42"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpcclient.ConnectWithTLSExpecting(ctx, listener.Addr().String(), &certificate, nil, &expected)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
		t.Fatal(err)
	}
	spec, ok := specForConnection("ownership", conn)
	if !ok {
		t.Fatal("actual verified mTLS connection was not eligible for broker Start")
	}
	wire, _ := json.Marshal(spec)
	encoded := base64.RawURLEncoding.EncodeToString(wire)
	before := agent.calls.Load()
	if elevated {
		// Observe the real Start launch boundary, without letting a mutant launch
		// a long-lived helper. /bin/false is an inert, immediately reaped process.
		oldExecutable := HelperExecutable
		defer func() { HelperExecutable = oldExecutable }()
		launched := false
		HelperExecutable = func() (string, error) { launched = true; return "/bin/false", nil }
		if err = Start("ownership", conn); err != nil {
			t.Fatal(err)
		}
		if launched {
			t.Fatal("elevated Start launched a helper for a foreign-owned user directory")
		}
		runCtx, runCancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = Run(runCtx, encoded, time.Second, nil)
		runCancel()
		if err == nil {
			t.Fatal("elevated Run bypassed ownership refusal")
		}
		if agent.calls.Load() != before {
			t.Fatal("elevated broker contacted upstream after ownership refusal")
		}
		if _, err = os.Lstat(filepath.Join(directory, "sessions")); !os.IsNotExist(err) {
			t.Fatalf("elevated broker planted session state: %v", err)
		}
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
			t.Fatalf("elevated attempt changed user config tree: %v %v", entries, err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	childCtx, childCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer childCancel()
	child := exec.CommandContext(childCtx, executable, "-test.run=^TestBrokerUnprivilegedOwnershipProcess$", "-test.v", "-test.timeout=15s")
	child.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + directory, "XDG_CACHE_HOME=" + directory, "WENDY_CONFIG_DIR=" + directory, "WENDY_SECRET_STORE=file", "WENDY_TLS_SESSION_STORE=off", "WENDY_BROKER_OWNER_SPEC=" + encoded, "WENDY_OWNER_UID=" + strconv.Itoa(uid)}
	if elevated {
		child.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}}
	}
	out, err := child.CombinedOutput()
	t.Logf("unprivileged broker process: %s", out)
	if err != nil || !strings.Contains(string(out), "UNPRIVILEGED_BROKER_RPC_COMPLETED") {
		t.Fatalf("real user broker did not recover after elevated refusal: %v", err)
	}
	if agent.calls.Load() < before+3 {
		t.Fatal("unprivileged broker did not send its probe and live RPCs over mTLS")
	}
	err = filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Sys().(*syscall.Stat_t).Uid != uint32(uid) {
			t.Errorf("foreign-owned artifact remained after user operation: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if elevated {
		t.Log("elevated Start/Run preserved user ownership; real user broker proxied mTLS RPCs and drained its socket")
	} else {
		t.Log("ordinary user process completed production broker mTLS and IPC round trip")
	}
}

func TestBrokerUnprivilegedOwnershipProcess(t *testing.T) {
	encoded := os.Getenv("WENDY_BROKER_OWNER_SPEC")
	if encoded == "" {
		t.Skip("owned subprocess only")
	}
	uid, err := strconv.Atoi(os.Getenv("WENDY_OWNER_UID"))
	if err != nil || uid <= 0 || os.Getuid() != uid || os.Geteuid() != uid {
		t.Fatal("wrong real/effective user")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var spec Spec
	if err = json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, encoded, time.Second, nil) }()
	directory, err := brokerDir()
	if err != nil {
		t.Fatal(err)
	}
	socket, state := spec.paths(directory)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err = os.Stat(state); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("user broker stopped before state publication: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("user broker never published state")
		}
		time.Sleep(10 * time.Millisecond)
	}
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer probeCancel()
	proxy, err := Connect(probeCtx, spec.Key, spec.Expected)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	for range 2 {
		response, err := proxy.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
		if err != nil || response.GetVersion() != "ownership-wire-fixture" {
			t.Fatalf("wrong live broker reply: %v %v", response, err)
		}
	}
	for _, path := range []string{directory, socket, state} {
		if err := validateOwnerPath(path, uid); err != nil {
			t.Fatal(err)
		}
	}
	proxy.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("user broker did not drain")
	}
	for _, path := range []string{socket, state} {
		if _, err = os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("broker left live session artifact %s: %v", path, err)
		}
	}
	t.Log("UNPRIVILEGED_BROKER_RPC_COMPLETED")
}

func validateOwnerPath(path string, uid int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Sys().(*syscall.Stat_t).Uid != uint32(uid) || info.Mode().Perm()&0077 != 0 {
		return &os.PathError{Op: "ownership", Path: path, Err: os.ErrPermission}
	}
	return nil
}
