package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func TestTunnelSessionReusesConnectionButAuthorizesEveryDial(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	type request struct{ address, method string }
	requests := make(chan request, 2)
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		p, _ := peer.FromContext(stream.Context())
		method, _ := grpc.MethodFromServerStream(stream)
		requests <- request{p.Addr.String(), method}
		return status.Error(codes.PermissionDenied, "test refusal")
	}))
	go srv.Serve(lis)
	defer srv.Stop()
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = lis.Addr().String()
	seedConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	id := "00000000-0000-4000-8000-000000000042"
	d := cloudDiscoveryDevice{v2: &cloudpbv2.Asset{Id: id}, key: id}
	var session TunnelSession
	session.SetActive(true)
	defer session.SetActive(false)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var address string
	for i := 0; i < 2; i++ {
		if _, err := d.openTunnel(session.Context(ctx), nil, auth, 50052, false); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("dial %d did not preserve authorization refusal: %v", i, err)
		}
		select {
		case req := <-requests:
			if req.method != pb.TunnelAuthorizationService_RequestTunnel_FullMethodName {
				t.Fatalf("unexpected authorization RPC: %s", req.method)
			}
			if i > 0 && req.address != address {
				t.Fatalf("authorization connection was recreated: %s -> %s", address, req.address)
			}
			address = req.address
		case <-ctx.Done():
			t.Fatal("dial did not request fresh authorization")
		}
	}
}

func TestTunnelSessionScopeAndLifetime(t *testing.T) {
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = "127.0.0.1:12345"
	var session TunnelSession
	session.SetActive(true)
	defer session.SetActive(false)
	old := session.Context(t.Context())
	first, release, err := tunnelAuthorizationFor(old, auth)
	if err != nil {
		t.Fatal(err)
	}
	release()
	same, release, err := tunnelAuthorizationFor(old, auth)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if first != same {
		t.Fatal("same credentials not reused")
	}
	changed := *auth
	changed.APIKey += "changed"
	different, release, err := tunnelAuthorizationFor(old, &changed)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if first == different {
		t.Fatal("changed credentials reused old authorization")
	}
	session.SetActive(false)
	if first.conn.GetState().String() != "SHUTDOWN" {
		t.Fatal("transport not closed")
	}
	session.SetActive(true)
	if _, _, err := tunnelAuthorizationFor(old, auth); err != context.Canceled {
		t.Fatalf("old handle resumed: %v", err)
	}
	fresh, release, err := tunnelAuthorizationFor(session.Context(t.Context()), auth)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if fresh == first {
		t.Fatal("new scope reused old resource")
	}
}

func TestTunnelSessionConcurrentReuse(t *testing.T) {
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = "127.0.0.1:12345"
	var session TunnelSession
	session.SetActive(true)
	defer session.SetActive(false)
	ctx := session.Context(t.Context())
	results := make(chan *tunnelAuthorization, 16)
	for range 16 {
		go func() {
			entry, release, err := tunnelAuthorizationFor(ctx, auth)
			if err != nil {
				t.Error(err)
				results <- nil
				return
			}
			defer release()
			results <- entry
		}()
	}
	first := <-results
	for range 15 {
		if got := <-results; got != first {
			t.Error("concurrent dials created separate transports")
		}
	}
}

func TestTunnelSessionPauseCancelsAuthorization(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		close(started)
		<-stream.Context().Done()
		return stream.Context().Err()
	}))
	go srv.Serve(lis)
	defer srv.Stop()
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = lis.Addr().String()
	seedConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	var session TunnelSession
	session.SetActive(true)
	defer session.SetActive(false)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = session.Context(ctx)
	id := "00000000-0000-4000-8000-000000000042"
	d := cloudDiscoveryDevice{v2: &cloudpbv2.Asset{Id: id}, key: id}
	done := make(chan error, 1)
	go func() { _, err := d.openTunnel(ctx, nil, auth, 50052, false); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("authorization did not start")
	}
	session.SetActive(false)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("paused authorization succeeded")
		}
	case <-ctx.Done():
		t.Fatal("pause did not cancel authorization")
	}
}

// A retained connection that failed to reconnect refuses requests until its
// backoff expires, long after Cloud is reachable again. A dial must not wait
// for that: it gets a connection that tries now, as an unshared dial would.
func TestTunnelSessionRecoversAsSoonAsCloudReturns(t *testing.T) {
	var authorizations atomic.Int32
	serve := func(address string) *grpc.Server {
		t.Helper()
		lis, err := net.Listen("tcp", address)
		for range 50 {
			if err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
			lis, err = net.Listen("tcp", address)
		}
		if err != nil {
			t.Fatal(err)
		}
		srv := grpc.NewServer(grpc.UnknownServiceHandler(func(any, grpc.ServerStream) error {
			authorizations.Add(1)
			return status.Error(codes.PermissionDenied, "test refusal")
		}))
		go srv.Serve(lis)
		return srv
	}
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := free.Addr().String()
	free.Close()
	srv := serve(address)
	defer func() { srv.Stop() }()
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = address
	seedConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	var session TunnelSession
	session.SetActive(true)
	defer session.SetActive(false)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ctx = session.Context(ctx)
	id := "00000000-0000-4000-8000-000000000042"
	d := cloudDiscoveryDevice{v2: &cloudpbv2.Asset{Id: id}, key: id}
	dial := func() codes.Code {
		_, err := d.openTunnel(ctx, nil, auth, 50052, false)
		return status.Code(err)
	}
	if got := dial(); got != codes.PermissionDenied {
		t.Fatalf("dial before the outage: %v", got)
	}
	srv.Stop()
	// Exercise the failed requests, then observe actual backoff. gRPC publishes
	// its error picker before its connectivity state, so Unavailable alone
	// does not guarantee that a retained connection is in TransientFailure.
	for range 2 {
		if got := dial(); got != codes.Unavailable {
			t.Fatalf("dial during the outage: %v", got)
		}
	}
	g := ctx.Value(tunnelContextKey{}).(*tunnelGeneration)
	g.mu.Lock()
	var retained *tunnelAuthorization
	for _, entry := range g.entries {
		retained = entry
	}
	g.mu.Unlock()
	if retained == nil {
		t.Fatal("outage did not retain an authorization transport")
	}
	for state := retained.conn.GetState(); state != connectivity.TransientFailure; state = retained.conn.GetState() {
		if state == connectivity.Shutdown || !retained.conn.WaitForStateChange(ctx, state) {
			t.Fatalf("retained transport did not enter backoff: state=%s error=%v", state, ctx.Err())
		}
	}
	// No successful-path retry: the first dial after a confirmed failed
	// reconnect must reach the returned Cloud server and its authorization.
	beforeRecovery := authorizations.Load()
	srv = serve(address)
	if got := dial(); got != codes.PermissionDenied {
		t.Fatalf("dial after Cloud returned: %v", got)
	}
	if got := authorizations.Load() - beforeRecovery; got != 1 {
		t.Fatalf("recovery sent %d authorization requests, want exactly one", got)
	}
	g.mu.Lock()
	var replacement *tunnelAuthorization
	for _, entry := range g.entries {
		replacement = entry
	}
	g.mu.Unlock()
	if replacement == nil || replacement == retained || replacement.conn == retained.conn {
		t.Fatal("recovery reused the failed authorization transport")
	}
	if got := retained.conn.GetState(); got != connectivity.Shutdown {
		t.Fatalf("replaced authorization transport was not closed: %s", got)
	}
}

// A signing pair supplied as files can be replaced on disk, so it is read for
// every dial instead of being retained with the session.
func TestTunnelSessionReadsSigningFilesOnEveryDial(t *testing.T) {
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = "127.0.0.1:12345"
	liteRelaySigningIdentity(t, auth.Certificates[0].PrincipalURI)
	var session TunnelSession
	session.SetActive(true)
	defer session.SetActive(false)
	ctx := session.Context(t.Context())
	_, release, err := tunnelAuthorizationFor(ctx, auth)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := os.Remove(os.Getenv("WENDY_TUNNEL_SIGNING_KEY")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tunnelAuthorizationFor(ctx, auth); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dial after the signing key was removed: %v", err)
	}
}

// WDY-4396: a held connection signs each dial with the login on disk now. An
// expired one fails locally; a renewed one is used without a restart.
func TestHeldCloudTunnelSignsWithLoginOnDisk(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	signedWith := make(chan []byte, 2)
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		var req pb.RequestTunnelRequest
		if err := stream.RecvMsg(&req); err != nil {
			return err
		}
		var header struct{ X5C [][]byte }
		raw, _ := base64.RawURLEncoding.DecodeString(strings.Split(string(req.GetPrincipalRequest().GetValue()), ".")[0])
		_ = json.Unmarshal(raw, &header)
		signedWith <- header.X5C[0]
		return status.Error(codes.PermissionDenied, "test refusal")
	}))
	go srv.Serve(lis)
	defer srv.Stop()
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = lis.Addr().String()
	auth.OAuthIssuer = "" // no sign-in re-issue: an expired leaf stays expired
	seedConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	held := *auth
	id := "00000000-0000-4000-8000-000000000042"
	d := cloudDiscoveryDevice{v2: &cloudpbv2.Asset{Id: id}, key: id}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	later := time.Now().Add(2 * time.Hour) // past the held leaf's NotAfter
	oldNow := timeNowFn
	t.Cleanup(func() { timeNowFn = oldNow })
	timeNowFn = func() time.Time { return later }
	if _, err := d.openTunnel(ctx, nil, &held, 50052, false); err == nil || !strings.Contains(err.Error(), "wendy auth login") {
		t.Fatalf("expired login: %v", err)
	}
	select {
	case <-signedWith:
		t.Fatal("expired login reached Cloud")
	default:
	}

	key, err := certs.ParseSigningPrivateKeyPEM([]byte(auth.Certificates[0].PemPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	renewed := *auth
	renewed.Certificates = []config.CertificateInfo{auth.Certificates[0]}
	renewed.Certificates[0].PemCertificate = testLeafPEMUntil(t, key, time.Now().Add(48*time.Hour))
	if err := config.Save(&config.Config{Auth: []config.AuthConfig{renewed}}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.openTunnel(ctx, nil, &held, 50052, false); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("renewed login: %v", err)
	}
	leaf, err := certs.ParseStoredLeaf(renewed.Certificates[0].PemCertificate)
	if err != nil {
		t.Fatal(err)
	}
	if got := <-signedWith; !bytes.Equal(got, leaf.Raw) {
		t.Fatal("held connection signed with its captured leaf, not the renewed one")
	}
}

// WDY-4396 review B1: a held connection's device mTLS presents the leaf it
// captured. Once that leaf expires, its redial stops before Cloud, and
// Reconnect rebuilds with the login on disk, or stops when there is none.
func TestHeldCloudConnectionReconnectsWithLoginOnDisk(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var cloudCalls atomic.Int32
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(any, grpc.ServerStream) error {
		cloudCalls.Add(1)
		return status.Error(codes.PermissionDenied, "test refusal")
	}))
	go srv.Serve(lis)
	defer srv.Stop()
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = lis.Addr().String()
	auth.OAuthIssuer = "" // no sign-in re-issue: an expired leaf stays expired
	auth.Certificates[0].PemCertificateChain = auth.Certificates[0].PemCertificate // any CA; no handshake runs
	held := *auth
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(auth.Certificates[0].PemPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	renewed := *auth
	renewed.Certificates = []config.CertificateInfo{auth.Certificates[0]}
	renewed.Certificates[0].PemCertificate = testLeafPEMUntil(t, key, time.Now().Add(48*time.Hour))
	seedConfig(t, &config.Config{Auth: []config.AuthConfig{renewed}})
	id := "00000000-0000-4000-8000-000000000042"
	d := cloudDiscoveryDevice{cloudAssetMetadata: &cloudpbv2.Asset{Id: id}, v2: &cloudpbv2.Asset{Id: id}, key: id}
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), cloudTunnelOnlyKey{}, true), 10*time.Second)
	defer cancel()
	conn, err := connectCloudDiscoveryTransport(ctx, &held, d, "")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	later := time.Now().Add(2 * time.Hour) // past the held leaf's NotAfter
	oldNow, oldDial := timeNowFn, desktopCloudDial
	t.Cleanup(func() { timeNowFn, desktopCloudDial = oldNow, oldDial })
	timeNowFn = func() time.Time { return later }
	rpcCtx, rpcCancel := context.WithTimeout(ctx, 2*time.Second)
	_, err = conn.AgentService.GetAgentVersion(rpcCtx, &agentpb.GetAgentVersionRequest{})
	rpcCancel()
	if err == nil || !strings.Contains(err.Error(), errHeldSignInExpired.Error()) {
		t.Fatalf("held connection with an expired leaf: %v", err)
	}

	var dialedWith string
	desktopCloudDial = func(_ context.Context, auth *config.AuthConfig, _ cloudDiscoveryDevice, _ string) (*grpcclient.AgentConnection, error) {
		dialedWith = auth.Certificates[0].PemCertificate
		return &grpcclient.AgentConnection{AgentService: &probeBudgetAgent{}}, nil
	}
	next, err := conn.Reconnect(ctx)
	if err != nil {
		t.Fatalf("reconnect with a fresh login on disk: %v", err)
	}
	next.Close()
	if dialedWith != renewed.Certificates[0].PemCertificate {
		t.Fatal("reconnect rebuilt the connection with its captured login, not the one on disk")
	}

	dialedWith = ""
	if err := config.Save(&config.Config{Auth: []config.AuthConfig{held}}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Reconnect(ctx); err == nil || err.Error() != "your certificate has expired; run 'wendy auth login' to sign in again" {
		t.Fatalf("reconnect with only the expired login: %v", err)
	}
	if dialedWith != "" {
		t.Fatal("reconnect dialed with the expired login")
	}
	if n := cloudCalls.Load(); n != 0 {
		t.Fatalf("Cloud got %d tunnel requests; want none", n)
	}
}

// WDY-4396 review N1: an expired login that is not the current context reads
// as one sentence with one step, like the current one.
func TestExpiredNonCurrentLoginGivesOneStep(t *testing.T) {
	held := *oidcEnrollmentAuth(t)
	held.Name = "a"
	other := *oidcEnrollmentAuth(t)
	other.Name = "b"
	other.CloudGRPC = "other.example:443"
	seedConfig(t, &config.Config{Auth: []config.AuthConfig{held, other}, CurrentContext: "b"})
	oldNow, oldFresh := timeNowFn, ensureFreshCertificateFn
	t.Cleanup(func() { timeNowFn, ensureFreshCertificateFn = oldNow, oldFresh })
	later := time.Now().Add(2 * time.Hour)
	timeNowFn = func() time.Time { return later }
	ensureFreshCertificateFn = func(context.Context, *config.AuthConfig) error { return nil }
	_, err := currentCloudLogin(t.Context(), &held)
	if err == nil || err.Error() != "your certificate has expired; run 'wendy auth login' to sign in again" {
		t.Fatalf("expired non-current login: %v", err)
	}
}
