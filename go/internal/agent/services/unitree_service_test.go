package services

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

const unitreeTestKey = "00112233445566778899aabbccddeeff"

type testUnitreeLink struct {
	online atomic.Bool
	done   chan struct{}
	once   sync.Once
}

func newTestUnitreeLink() *testUnitreeLink {
	c := &testUnitreeLink{done: make(chan struct{})}
	c.online.Store(true)
	return c
}
func (c *testUnitreeLink) Connected() bool       { return c.online.Load() }
func (c *testUnitreeLink) Done() <-chan struct{} { return c.done }
func (c *testUnitreeLink) Close() error {
	c.once.Do(func() { c.online.Store(false); close(c.done) })
	return nil
}

func unitreeTestContext() context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{Addr: &net.UnixAddr{Name: "test", Net: "unix"}})
}

func testUnitreeService(t *testing.T) *UnitreeService {
	t.Helper()
	s := NewUnitreeService(context.Background(), filepath.Join(t.TempDir(), "pairings.json"), unitree.PairingOwner{OrgID: 42})
	s.retry = time.Hour
	t.Cleanup(s.Close)
	return s
}

func unitreeTestRequest() *agentpbv2.PairUnitreeRequest {
	return &agentpbv2.PairUnitreeRequest{Serial: "GO2TEST1", Name: "My Go2", Address: "192.0.2.20", EncryptionKey: unitreeTestKey, Token: unitree.AccountFlowMarker}
}

func TestUnitreePairRequiresAuthenticatedAgentTransport(t *testing.T) {
	s := testUnitreeService(t)
	s.dial = func(context.Context, string, string, string) (unitreeLink, error) {
		t.Fatal("unauthenticated request reached robot")
		return nil, nil
	}
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 50051}})
	if _, err := s.PairUnitree(ctx, unitreeTestRequest()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("plaintext pairing: %v", err)
	}
	if _, err := s.ListUnitreePairings(ctx, nil); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("plaintext list: %v", err)
	}
	if _, err := s.DiscoverUnitreeRobots(ctx, nil); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("plaintext discovery: %v", err)
	}
	if _, err := s.ForgetUnitree(ctx, nil); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("plaintext forget: %v", err)
	}
}

func TestUnitreeTransportRequiresCompletedMutualTLS(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state tls.ConnectionState
		ok    bool
	}{
		{"mutual TLS", tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{{}}}, true},
		{"server TLS only", tls.ConnectionState{HandshakeComplete: true}, false},
		{"incomplete handshake", tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := peer.NewContext(context.Background(), &peer.Peer{
				Addr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1)}, AuthInfo: credentials.TLSInfo{State: tc.state},
			})
			err := requireUnitreeTransport(ctx)
			if (err == nil) != tc.ok {
				t.Fatalf("transport authorization: %v", err)
			}
		})
	}
}

func unitreeTLSContext(t *testing.T, identities ...string) context.Context {
	t.Helper()
	leaf := &x509.Certificate{SerialNumber: big.NewInt(1), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	for _, identity := range identities {
		u, err := url.Parse(identity)
		if err != nil {
			t.Fatal(err)
		}
		leaf.URIs = append(leaf.URIs, u)
	}
	return peer.NewContext(context.Background(), &peer.Peer{
		Addr:     &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1)},
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{leaf}}},
	})
}

func TestUnitreeRPCsRequireOwnerScopeEvenWhenListenerAllowsCaller(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     interceptor.OrgMode
		identity string
	}{
		{"foreign tenant with off", interceptor.OrgModeOff, "urn:wendy:org:43:user:fixture"},
		{"identityless with grace", interceptor.OrgModeGrace, ""},
		{"non-comparable with grace", interceptor.OrgModeGrace, certs.UserSPIFFEURI("3fd28c47-700a-4ef6-9f43-19be3d13418e", "fixture")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testUnitreeService(t)
			if err := s.Rebind(unitree.PairingOwner{OrgID: 42}); err != nil {
				t.Fatal(err)
			}
			if err := s.store.Save(unitree.Pairing{Device: unitreeTestDevice(), PairedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			ctx := unitreeTLSContext(t, tc.identity)
			if err := interceptor.CheckMTLS(ctx, zap.NewNop(), certs.Scope{OrgID: 42}, tc.mode); err != nil {
				t.Fatal("fixture did not demonstrate listener bypass", err)
			}
			s.dial = func(context.Context, string, string, string) (unitreeLink, error) {
				t.Error("foreign owner reached robot dialing")
				return nil, errors.New("must not dial")
			}
			s.discover = func(context.Context, ...string) ([]unitree.Device, error) {
				t.Error("foreign owner reached discovery")
				return nil, nil
			}
			_, listErr := s.ListUnitreePairings(ctx, nil)
			_, pairErr := s.PairUnitree(ctx, unitreeTestRequest())
			_, discoveryErr := s.DiscoverUnitreeRobots(ctx, nil)
			_, forgetErr := s.ForgetUnitree(ctx, &agentpbv2.ForgetUnitreeRequest{Serial: "GO2TEST1"})
			for _, err := range []error{listErr, pairErr, discoveryErr, forgetErr} {
				if status.Code(err) != codes.PermissionDenied {
					t.Errorf("owner scope was not enforced: %v", err)
				}
			}
			if saved, err := s.store.List(); err != nil || len(saved) != 1 {
				t.Fatal("foreign caller changed saved credentials", err)
			}
		})
	}
}

func unitreeTestDevice() unitree.Device {
	return unitree.Device{Serial: "GO2TEST1", Address: "192.0.2.20", EncryptionKey: unitreeTestKey}
}

func TestUnitreeTLSOwnerScope(t *testing.T) {
	const tenant = "3fd28c47-700a-4ef6-9f43-19be3d13418e"
	for _, tc := range []struct {
		name       string
		owner      unitree.PairingOwner
		identities []string
		allowed    bool
	}{
		{"legacy match", unitree.PairingOwner{OrgID: 42}, []string{"urn:wendy:org:42:user:fixture"}, true},
		{"SPIFFE match", unitree.PairingOwner{TenantUUID: tenant}, []string{certs.UserSPIFFEURI(tenant, "fixture")}, true},
		{"SPIFFE mismatch", unitree.PairingOwner{TenantUUID: tenant}, []string{certs.UserSPIFFEURI("72900574-08b3-446c-a5d8-59b35264069e", "fixture")}, false},
		{"legacy mismatch", unitree.PairingOwner{OrgID: 42}, []string{"urn:wendy:org:43:user:fixture"}, false},
		{"malformed identity", unitree.PairingOwner{OrgID: 42}, []string{"urn:wendy:org:0:user:fixture"}, false},
		{"ambiguous identity", unitree.PairingOwner{OrgID: 42}, []string{"urn:wendy:org:42:user:fixture", "urn:wendy:org:43:user:fixture"}, false},
		{"local owner", unitree.PairingOwner{Local: true}, []string{"urn:wendy:org:42:user:fixture"}, false},
		{"unknown owner", unitree.PairingOwner{}, []string{"urn:wendy:org:42:user:fixture"}, false},
		{"dual accepts same legacy org", unitree.PairingOwner{TenantUUID: tenant, OrgID: 42}, []string{"urn:wendy:org:42:user:fixture"}, true},
		{"dual accepts same UUID", unitree.PairingOwner{TenantUUID: tenant, OrgID: 42}, []string{certs.UserSPIFFEURI(tenant, "fixture")}, true},
		{"authoritative UUID", unitree.PairingOwner{TenantUUID: tenant, OrgID: 42}, []string{certs.UserSPIFFEURI("72900574-08b3-446c-a5d8-59b35264069e", "fixture"), "urn:wendy:org:42:user:fixture"}, false},
		{"UUID-only denies unmapped legacy", unitree.PairingOwner{TenantUUID: tenant}, []string{"urn:wendy:org:42:user:fixture"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testUnitreeService(t)
			if err := s.Rebind(tc.owner); err != nil {
				t.Fatal(err)
			}
			_, err := s.ListUnitreePairings(unitreeTLSContext(t, tc.identities...), nil)
			if tc.allowed && err != nil || !tc.allowed && status.Code(err) != codes.PermissionDenied {
				t.Fatalf("scope authorization: %v", err)
			}
		})
	}
}

func TestUnitreePairValidatesBeforeSavingAndRetainsSession(t *testing.T) {
	s := testUnitreeService(t)
	link := newTestUnitreeLink()
	calls := 0
	s.dial = func(ctx context.Context, address, token, key string) (unitreeLink, error) {
		calls++
		if address != "192.0.2.20" || token != "" || key != unitreeTestKey {
			t.Fatal("pairing credentials or address were lost")
		}
		if saved, _ := s.store.List(); len(saved) != 0 {
			t.Fatal("saved before validation")
		}
		return link, nil
	}
	ctx, cancel := context.WithCancel(unitreeTestContext())
	resp, err := s.PairUnitree(ctx, unitreeTestRequest())
	if err != nil || !resp.GetRobot().GetConnected() {
		t.Fatalf("pair: %v, %v", resp, err)
	}
	cancel()
	if !link.Connected() {
		t.Fatal("RPC cancellation closed retained connection")
	}
	saved, err := s.store.List()
	if err != nil || len(saved) != 1 || saved[0].Device.EncryptionKey != unitreeTestKey {
		t.Fatalf("saved pairing: %v", err)
	}
	listed, err := s.ListUnitreePairings(unitreeTestContext(), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := protojson.Marshal(listed)
	if strings.Contains(string(raw), unitreeTestKey) || strings.Contains(string(raw), "cloud-token-secret") {
		t.Fatal("RPC exposed secrets")
	}
	req := unitreeTestRequest()
	req.Token, req.EncryptionKey = "", ""
	if _, err := s.PairUnitree(unitreeTestContext(), req); err != nil || calls != 1 {
		t.Fatalf("existing connection was not reused: calls=%d err=%v", calls, err)
	}
	if _, err := s.ForgetUnitree(unitreeTestContext(), &agentpbv2.ForgetUnitreeRequest{Serial: " " + req.Serial + " "}); err != nil {
		t.Fatal(err)
	}
	if link.Connected() {
		t.Fatal("forget left connection open")
	}
	if saved, _ := s.store.List(); len(saved) != 0 {
		t.Fatal("forget retained robot key")
	}
}

func TestUnitreeFailedPairDoesNotSaveOrExposeSecrets(t *testing.T) {
	s := testUnitreeService(t)
	s.dial = func(context.Context, string, string, string) (unitreeLink, error) {
		return nil, errors.New("remote error " + unitreeTestKey + " cloud-token-secret")
	}
	_, err := s.PairUnitree(unitreeTestContext(), unitreeTestRequest())
	if status.Code(err) != codes.Unavailable || strings.Contains(err.Error(), unitreeTestKey) || strings.Contains(err.Error(), "cloud-token-secret") {
		t.Fatalf("unsafe pairing error: %v", err)
	}
	if saved, _ := s.store.List(); len(saved) != 0 {
		t.Fatal("failed pairing was saved")
	}
}

func TestUnitreeEnrollmentChangeClearsCredentialsAndClosesSessions(t *testing.T) {
	s := testUnitreeService(t)
	link := newTestUnitreeLink()
	s.dial = func(context.Context, string, string, string) (unitreeLink, error) { return link, nil }
	if _, err := s.PairUnitree(unitreeTestContext(), unitreeTestRequest()); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebind(unitree.PairingOwner{OrgID: 42}); err != nil || !link.Connected() {
		t.Fatal("same-owner renewal dropped a pairing", err)
	}
	if err := s.Rebind(unitree.PairingOwner{}); err != nil {
		t.Fatal(err)
	}
	if link.Connected() {
		t.Fatal("ownership reset retained a robot session")
	}
	if _, err := s.ListUnitreePairings(unitreeTestContext(), &agentpbv2.ListUnitreePairingsRequest{}); status.Code(err) != codes.Internal {
		t.Fatalf("disabled ownership allowed pairing access: %v", err)
	}
	if err := s.Rebind(unitree.PairingOwner{OrgID: 43}); err != nil {
		t.Fatal(err)
	}
	list, err := s.store.List()
	if err != nil || len(list) != 0 {
		t.Fatal("new tenant inherited saved keys", err)
	}
}

func TestUnitreeDualScopeRenewalPreservesKeysAndCurrentAuthorization(t *testing.T) {
	const tenant = "3fd28c47-700a-4ef6-9f43-19be3d13418e"
	s := testUnitreeService(t)
	link := newTestUnitreeLink()
	s.dial = func(context.Context, string, string, string) (unitreeLink, error) { return link, nil }
	if _, err := s.PairUnitree(unitreeTestContext(), unitreeTestRequest()); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebind(unitree.PairingOwner{OrgID: 42, TenantUUID: tenant}); err != nil {
		t.Fatal(err)
	}
	if !link.Connected() {
		t.Fatal("dual-SAN renewal dropped a same-tenant session")
	}
	legacyCtx := unitreeTLSContext(t, "urn:wendy:org:42:user:fixture")
	if list, err := s.ListUnitreePairings(legacyCtx, nil); err != nil || len(list.GetRobots()) != 1 {
		t.Fatal("current dual scope refused a same-org legacy caller", err)
	}
	if err := s.Rebind(unitree.PairingOwner{TenantUUID: tenant}); err != nil {
		t.Fatal(err)
	}
	if !link.Connected() {
		t.Fatal("UUID-only renewal dropped the retained session")
	}
	// Persisting proven old vocabulary must not widen CURRENT RPC authority.
	if _, err := s.ListUnitreePairings(legacyCtx, nil); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("stored legacy org overrode the current UUID-only scope: %v", err)
	}
	if list, err := s.ListUnitreePairings(unitreeTLSContext(t, certs.UserSPIFFEURI(tenant, "fixture")), nil); err != nil || len(list.GetRobots()) != 1 {
		t.Fatal("UUID-only renewal lost current-tenant pairing access", err)
	}
	if err := s.Rebind(unitree.PairingOwner{TenantUUID: "72900574-08b3-446c-a5d8-59b35264069e", OrgID: 42}); err != nil {
		t.Fatal(err)
	}
	if link.Connected() {
		t.Fatal("different UUID retained another tenant's session")
	}
	if saved, err := s.store.List(); err != nil || len(saved) != 0 {
		t.Fatal("different UUID inherited a key despite its matching legacy org", err)
	}
}

func TestUnitreeOrgOnlyRenewalCannotEraseKnownSessionOwnership(t *testing.T) {
	s := testUnitreeService(t)
	link := newTestUnitreeLink()
	s.dial = func(context.Context, string, string, string) (unitreeLink, error) { return link, nil }
	if _, err := s.PairUnitree(unitreeTestContext(), unitreeTestRequest()); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []unitree.PairingOwner{
		{OrgID: 42, TenantUUID: "3fd28c47-700a-4ef6-9f43-19be3d13418e"},
		{OrgID: 42},
	} {
		if err := s.Rebind(owner); err != nil || !link.Connected() {
			t.Fatal("proven same-tenant renewal lost the session", err)
		}
	}
	if err := s.Rebind(unitree.PairingOwner{OrgID: 42, TenantUUID: "72900574-08b3-446c-a5d8-59b35264069e"}); err == nil {
		t.Fatal("known UUID conflict was hidden behind the legacy org")
	}
	if link.Connected() {
		t.Fatal("ownership uncertainty retained old robot credentials in a session")
	}
	if _, err := s.store.List(); err == nil {
		t.Fatal("new UUID could read the previous tenant's keys")
	}
}

func TestUnitreeEnrollmentChangeFailsClosedWhenClearFails(t *testing.T) {
	s := testUnitreeService(t)
	link := newTestUnitreeLink()
	s.dial = func(context.Context, string, string, string) (unitreeLink, error) { return link, nil }
	if _, err := s.PairUnitree(unitreeTestContext(), unitreeTestRequest()); err != nil {
		t.Fatal(err)
	}
	s.store = clearFailingUnitreeStore{s.store}
	if err := s.Rebind(unitree.PairingOwner{OrgID: 43}); err == nil {
		t.Fatal("failed clearing was not reported")
	}
	if link.Connected() {
		t.Fatal("failed clear left an old robot session active")
	}
	if list, err := s.store.List(); err == nil || len(list) != 0 {
		t.Fatal("new tenant could read old keys after failed clearing")
	}
	s.dial = func(context.Context, string, string, string) (unitreeLink, error) {
		t.Error("restore dialed another tenant's robot")
		return nil, errors.New("must not dial")
	}
	if err := s.Restore(); err == nil {
		t.Fatal("restore accepted another tenant's credentials")
	}
}

func TestUnitreeFailedClearIsRetriedOnSameOwnerRebind(t *testing.T) {
	s := testUnitreeService(t)
	s.dial = func(context.Context, string, string, string) (unitreeLink, error) { return newTestUnitreeLink(), nil }
	if _, err := s.PairUnitree(unitreeTestContext(), unitreeTestRequest()); err != nil {
		t.Fatal(err)
	}
	failing := &flakyClearUnitreeStore{unitreePairingStore: s.store, failures: 1}
	s.store = failing
	if err := s.Rebind(unitree.PairingOwner{OrgID: 43}); err == nil {
		t.Fatal("failed clearing was not reported")
	}
	if err := s.Rebind(unitree.PairingOwner{OrgID: 43}); err != nil {
		t.Fatal("same-owner renewal did not retry clearing the previous owner's keys", err)
	}
	if failing.calls != 2 {
		t.Fatalf("clear attempts = %d, want 2", failing.calls)
	}
	if list, err := s.store.List(); err != nil || len(list) != 0 {
		t.Fatal("new owner cannot use pairing after retried clear", err)
	}
	if err := s.Rebind(unitree.PairingOwner{OrgID: 43}); err != nil || failing.calls != 2 {
		t.Fatal("healthy same-owner renewal cleared pairings again", err)
	}
}

type flakyClearUnitreeStore struct {
	unitreePairingStore
	failures, calls int
}

func (s *flakyClearUnitreeStore) Clear() error {
	s.calls++
	if s.calls <= s.failures {
		return errors.New("fixture clear failure")
	}
	return s.unitreePairingStore.Clear()
}

func TestUnitreeUnenrolledRestoreClearsPreviousEnrollmentKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pairings.json")
	saved := unitree.Pairing{Device: unitree.Device{Serial: "GO2TEST1", Address: "192.0.2.20", EncryptionKey: unitreeTestKey}, PairedAt: time.Now()}
	if err := unitree.NewStore(path, unitree.PairingOwner{OrgID: 42}).Save(saved); err != nil {
		t.Fatal(err)
	}
	noDial := func(context.Context, string, string, string) (unitreeLink, error) {
		t.Error("restore dialed a previous enrollment's robot")
		return nil, errors.New("must not dial")
	}

	// Another enrolled tenant fails closed without deleting: ownership
	// differences at boot are not proof the file is abandoned.
	other := NewUnitreeService(context.Background(), path, unitree.PairingOwner{OrgID: 43})
	other.dial = noDial
	if err := other.Restore(); err == nil {
		t.Fatal("restore accepted another tenant's credentials")
	}
	other.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("enrolled restore removed a differently owned store", err)
	}

	local := NewUnitreeService(context.Background(), path, unitree.PairingOwner{Local: true})
	local.dial = noDial
	t.Cleanup(local.Close)
	if err := local.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("unenrolled restore retained a previous enrollment's keys", err)
	}
	if list, err := local.store.List(); err != nil || len(list) != 0 {
		t.Fatal("local pairing unusable after clearing", err)
	}
}

func TestUnitreeRetainedLinkDropWaitsBeforeRedial(t *testing.T) {
	s := testUnitreeService(t)
	first := newTestUnitreeLink()
	var dials atomic.Int32
	s.dial = func(context.Context, string, string, string) (unitreeLink, error) {
		if dials.Add(1) == 1 {
			return first, nil
		}
		return newTestUnitreeLink(), nil
	}
	if _, err := s.PairUnitree(unitreeTestContext(), unitreeTestRequest()); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	time.Sleep(100 * time.Millisecond)
	if n := dials.Load(); n != 1 {
		t.Fatalf("dropped link redialed immediately: %d dials", n)
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("retry wait after a dropped link was not cancellable")
	}
}

type clearFailingUnitreeStore struct{ unitreePairingStore }

func (s clearFailingUnitreeStore) Clear() error { return errors.New("fixture clear failure") }

type failingUnitreeStore struct{ unitreePairingStore }

func (s failingUnitreeStore) Save(unitree.Pairing) error { return errors.New("disk full") }

func TestUnitreeSaveFailureClosesNewConnection(t *testing.T) {
	s := testUnitreeService(t)
	s.store = failingUnitreeStore{s.store}
	link := newTestUnitreeLink()
	s.dial = func(context.Context, string, string, string) (unitreeLink, error) { return link, nil }
	if _, err := s.PairUnitree(unitreeTestContext(), unitreeTestRequest()); status.Code(err) != codes.Internal {
		t.Fatalf("save: %v", err)
	}
	if link.Connected() {
		t.Fatal("unsaved connection leaked")
	}
}

func TestUnitreeRestoreUsesRobotKeyWithoutCloudToken(t *testing.T) {
	s := testUnitreeService(t)
	if err := s.store.Save(unitree.Pairing{Device: unitree.Device{Serial: "GO2TEST1", Address: "192.0.2.20", EncryptionKey: unitreeTestKey}, PairedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	type attempt struct{ address, token, key string }
	attempted := make(chan attempt, 2)
	link := newTestUnitreeLink()
	s.dial = func(ctx context.Context, address, token, key string) (unitreeLink, error) {
		attempted <- attempt{address, token, key}
		return link, nil
	}
	if err := s.Restore(); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-attempted:
		if a.address != "192.0.2.20" || a.token != "" || a.key != unitreeTestKey {
			t.Fatal("restore did not use saved robot key")
		}
	case <-time.After(time.Second):
		t.Fatal("restore did not connect")
	}
	s.Close()
	if link.Connected() {
		t.Fatal("shutdown did not close robot connection")
	}
}

func TestUnitreeDiscoveryStopsDuringShutdown(t *testing.T) {
	s := testUnitreeService(t)
	started := make(chan struct{})
	done := make(chan error, 1)
	s.discover = func(ctx context.Context, _ ...string) ([]unitree.Device, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	go func() {
		_, err := s.DiscoverUnitreeRobots(unitreeTestContext(), nil)
		done <- err
	}()
	<-started
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown waited for discovery's full request timeout")
	}
	if err := <-done; status.Code(err) != codes.Unavailable {
		t.Fatalf("cancelled discovery: %v", err)
	}
	if _, err := s.DiscoverUnitreeRobots(unitreeTestContext(), nil); status.Code(err) != codes.Unavailable {
		t.Fatalf("discovery after shutdown: %v", err)
	}
}

func TestUnitreeRestoreBoundsConcurrentDials(t *testing.T) {
	s := testUnitreeService(t)
	const count = unitreeMaxParallelDials * 2
	for i := range count {
		d := unitreeTestDevice()
		d.Serial = fmt.Sprintf("GO2TEST%d", i)
		if err := s.store.Save(unitree.Pairing{Device: d, PairedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan struct{}, count)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var active atomic.Int32
	s.dial = func(ctx context.Context, _, _, _ string) (unitreeLink, error) {
		if active.Add(1) > unitreeMaxParallelDials {
			t.Error("restore exceeded the expensive setup concurrency bound")
		}
		defer active.Add(-1)
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return newTestUnitreeLink(), nil
		}
	}
	if err := s.Restore(); err != nil {
		t.Fatal(err)
	}
	for range unitreeMaxParallelDials {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("initial bounded restoration did not start")
		}
	}
	releaseOnce.Do(func() { close(release) })
	for range count - unitreeMaxParallelDials {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("queued restoration did not progress")
		}
	}
	s.Close()
}

func TestUnitreeCancelledDialDoesNotReachRobot(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprint(full), func(t *testing.T) {
			s := testUnitreeService(t)
			if full {
				for range cap(s.dialSlots) {
					s.dialSlots <- struct{}{}
				}
			}
			s.dial = func(context.Context, string, string, string) (unitreeLink, error) {
				t.Error("cancelled setup reached robot dialing")
				return nil, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := s.connect(ctx, "192.0.2.20", "", unitreeTestKey); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled queued setup: %v", err)
			}
		})
	}
}

func TestUnitreeRetryJitterIsBounded(t *testing.T) {
	for _, base := range []time.Duration{0, time.Millisecond, 10 * time.Second, time.Hour} {
		for range 100 {
			delay := unitreeRetryDelay(base)
			minimum := max(base, time.Millisecond)
			if delay < minimum || delay > minimum+minimum/2 {
				t.Fatalf("retry delay %v outside bounds for %v", delay, base)
			}
		}
	}
}

func TestUnitreePairRejectsInvalidAddressAndMissingKey(t *testing.T) {
	s := testUnitreeService(t)
	s.dial = func(context.Context, string, string, string) (unitreeLink, error) {
		t.Fatal("invalid input reached network")
		return nil, nil
	}
	for _, address := range []string{"", "go2.local", "127.0.0.1", "0.0.0.0", "224.0.0.1", "255.255.255.255", "::ffff:255.255.255.255", "::ffff:127.0.0.1", "::ffff:192.0.2.20", "192.0.2.2:9991", "http://192.0.2.2", "::1"} {
		req := unitreeTestRequest()
		req.Address = address
		if _, err := s.PairUnitree(unitreeTestContext(), req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("address %q: %v", address, err)
		}
	}
	req := unitreeTestRequest()
	req.EncryptionKey = ""
	req.Token = ""
	if _, err := s.PairUnitree(unitreeTestContext(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing key: %v", err)
	}
	req.EncryptionKey = "invalid-nonempty-key"
	req.Token = unitree.AccountFlowMarker
	if _, err := s.PairUnitree(unitreeTestContext(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("malformed key: %v", err)
	}
}

func TestUnitreePairRejectsSecretTokenAndReportsFieldErrors(t *testing.T) {
	s := testUnitreeService(t)
	s.dial = func(context.Context, string, string, string) (unitreeLink, error) {
		t.Fatal("invalid request consumed a robot connection slot")
		return nil, nil
	}
	req := unitreeTestRequest()
	req.Token = "cloud-token-secret"
	_, err := s.PairUnitree(unitreeTestContext(), req)
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "account-flow marker") || strings.Contains(err.Error(), "cloud-token-secret") {
		t.Fatalf("account token accepted or echoed: %v", err)
	}
	req = unitreeTestRequest()
	req.Name = strings.Repeat("n", 257)
	if _, err := s.PairUnitree(unitreeTestContext(), req); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "name") {
		t.Fatalf("long name reported as another field: %v", err)
	}
}

func TestUnitreePairRejectsInvalidSerialBeforeDialing(t *testing.T) {
	s := testUnitreeService(t)
	s.dial = func(context.Context, string, string, string) (unitreeLink, error) {
		t.Fatal("malformed serial consumed a robot connection slot")
		return nil, nil
	}
	for _, serial := range []string{"", " ", "GO2\x00TEST", "GO2\x1b[31m", "GO2☃", "GO2 TEST", strings.Repeat("A", 129)} {
		req := unitreeTestRequest()
		req.Serial = serial
		if _, err := s.PairUnitree(unitreeTestContext(), req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("serial %q: %v", serial, err)
		}
	}
	if saved, err := s.store.List(); err != nil || len(saved) != 0 {
		t.Fatalf("invalid serial changed storage: %v", err)
	}
}

func TestUnitreeLegacyPairUsesMarkerWithoutDeviceKey(t *testing.T) {
	s := testUnitreeService(t)
	link := newTestUnitreeLink()
	s.dial = func(_ context.Context, _ string, token, key string) (unitreeLink, error) {
		if token != "" || key != "" {
			t.Fatal("legacy pairing forwarded the marker or invented a key")
		}
		return link, nil
	}
	req := unitreeTestRequest()
	req.EncryptionKey = ""
	if _, err := s.PairUnitree(unitreeTestContext(), req); err != nil {
		t.Fatal(err)
	}
	saved, err := s.store.List()
	if err != nil || len(saved) != 1 || saved[0].Device.EncryptionKey != "" {
		t.Fatalf("legacy pairing was not saved: %v", err)
	}
	req.Token = ""
	if _, err := s.PairUnitree(unitreeTestContext(), req); err != nil {
		t.Fatalf("existing legacy pairing was not reusable: %v", err)
	}
}

func TestUnitreeManualPairWaitsForBackgroundDialCleanup(t *testing.T) {
	s := testUnitreeService(t)
	previous := unitree.Pairing{Device: unitree.Device{Serial: "GO2TEST1", Address: "192.0.2.20", EncryptionKey: unitreeTestKey}, PairedAt: time.Now()}
	if err := s.store.Save(previous); err != nil {
		t.Fatal(err)
	}
	started, canceled, cleaned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	manualStarted := make(chan struct{}, 1)
	link := newTestUnitreeLink()
	// Neither dial carries a token, so tell them apart by order: the first is
	// the background reconnect started by Restore.
	var dials atomic.Int32
	s.dial = func(ctx context.Context, _ string, _, _ string) (unitreeLink, error) {
		if dials.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
			close(cleaned)
			return nil, ctx.Err()
		}
		select {
		case <-cleaned:
		default:
			t.Error("manual pairing overlapped a background dial")
			return nil, errors.New("client slot still occupied")
		}
		manualStarted <- struct{}{}
		return link, nil
	}
	if err := s.Restore(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("background reconnect did not start")
	}
	paired := make(chan error, 1)
	go func() {
		_, err := s.PairUnitree(unitreeTestContext(), unitreeTestRequest())
		paired <- err
	}()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("manual pairing did not cancel the background dial")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-paired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("manual pairing did not finish")
	}
	select {
	case <-manualStarted:
	default:
		t.Fatal("manual connection was not established")
	}
}

func TestUnitreeFailedManualPairRestoresPreviousSession(t *testing.T) {
	s := testUnitreeService(t)
	oldLink, restoredLink := newTestUnitreeLink(), newTestUnitreeLink()
	restored := make(chan struct{}, 1)
	// Dials: 1 = first manual pairing, 2 = failing replacement, 3+ = recovery.
	var dials atomic.Int32
	s.dial = func(_ context.Context, address, _, key string) (unitreeLink, error) {
		n := dials.Add(1)
		if n >= 3 {
			if address != "192.0.2.20" || key != unitreeTestKey {
				t.Error("recovery did not use the previous pairing")
			}
			restored <- struct{}{}
			return restoredLink, nil
		}
		if n == 1 {
			return oldLink, nil
		}
		if oldLink.Connected() {
			t.Error("manual connection started before the old client slot was released")
		}
		return nil, errors.New("replacement endpoint unreachable")
	}
	req := unitreeTestRequest()
	if _, err := s.PairUnitree(unitreeTestContext(), req); err != nil {
		t.Fatal(err)
	}
	req.Address = "192.0.2.21"
	if _, err := s.PairUnitree(unitreeTestContext(), req); status.Code(err) != codes.Unavailable {
		t.Fatalf("replacement pairing: %v", err)
	}
	select {
	case <-restored:
	case <-time.After(2 * time.Second):
		t.Fatal("failed manual pairing did not resume the previous session")
	}
	saved, err := s.store.List()
	if err != nil || len(saved) != 1 || saved[0].Device.Address != "192.0.2.20" {
		t.Fatalf("failed pairing replaced the saved endpoint: %v", err)
	}
	s.Close()
	if restoredLink.Connected() {
		t.Fatal("restored connection was not owned by the service")
	}
}

func TestUnitreeDiscoveryRunsOnAgentAndReturnsNoSecrets(t *testing.T) {
	s := testUnitreeService(t)
	s.discover = func(ctx context.Context, serials ...string) ([]unitree.Device, error) {
		if len(serials) != 1 || serials[0] != "GO2TEST1" {
			t.Fatal("known serial missing")
		}
		return []unitree.Device{{Serial: "GO2TEST1", Address: "192.0.2.20", EncryptionKey: unitreeTestKey}}, nil
	}
	resp, err := s.DiscoverUnitreeRobots(unitreeTestContext(), &agentpbv2.DiscoverUnitreeRobotsRequest{KnownSerials: []string{"GO2TEST1"}})
	if err != nil || len(resp.GetRobots()) != 1 || resp.Robots[0].Connected || resp.Robots[0].Paired {
		t.Fatalf("discovery: %v %v", resp, err)
	}
	raw, _ := protojson.Marshal(resp)
	if strings.Contains(string(raw), unitreeTestKey) {
		t.Fatal("discovery exposed key")
	}
}
