package devices

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
)

type fakeConnector func(context.Context, platform.DeviceConnectRequest) (platform.DeviceConnection, error)

func (f fakeConnector) ConnectDevice(ctx context.Context, request platform.DeviceConnectRequest) (platform.DeviceConnection, error) {
	return f(ctx, request)
}

type fakeConnection struct {
	platform.Transport
	probe    func(context.Context, platform.RPCRequest) (platform.RPCResponse, error)
	evidence platform.DeviceConnectionEvidence
	closed   atomic.Int32
}

func (c *fakeConnection) Unary(ctx context.Context, request platform.RPCRequest) (platform.RPCResponse, error) {
	return c.probe(ctx, request)
}
func (c *fakeConnection) Evidence() platform.DeviceConnectionEvidence { return c.evidence }
func (c *fakeConnection) Close() error                                { c.closed.Add(1); return nil }
func failingConnection(err error) *fakeConnection {
	return &fakeConnection{probe: func(context.Context, platform.RPCRequest) (platform.RPCResponse, error) {
		return platform.RPCResponse{}, err
	}}
}
func workingConnection() *fakeConnection {
	return &fakeConnection{probe: func(_ context.Context, request platform.RPCRequest) (platform.RPCResponse, error) {
		if request.Method != "/wendy.agent.services.v1.WendyAgentService/GetAgentVersion" {
			panic(request.Method)
		}
		bytes, _ := proto.Marshal(&AgentVersion{Version: "test", Hostname: "pi"})
		return platform.RPCResponse{Message: bytes}, nil
	}}
}

func TestDialCredentialsAndPorts(t *testing.T) {
	var requests []platform.DeviceConnectRequest
	winner := workingConnection()
	rejected := failingConnection(errors.New("authentication handshake failed: first record does not look like a TLS handshake"))
	connector := fakeConnector(func(_ context.Context, request platform.DeviceConnectRequest) (platform.DeviceConnection, error) {
		requests = append(requests, request)
		if request.Address == "10.0.0.1:50052" {
			return winner, nil
		}
		return rejected, nil
	})
	expected := &platform.DeviceIdentity{OrgID: 7, EntityID: "pi"}
	result := Dial(context.Background(), connector, Target{Addr: "10.0.0.1:50051", Expected: expected}, []Credential{{Reference: "old", OrganizationID: 8}, {Reference: "preferred", OrganizationID: 7}}, DialOptions{PreferredOrg: 7, HavePreferredOrg: true})
	if result.Err != nil || result.Connection != winner {
		t.Fatalf("Dial = %+v", result)
	}
	if got := []string{requests[0].Credential, requests[1].Credential, requests[2].Credential}; !reflect.DeepEqual(got, []string{"preferred", "old", "preferred"}) {
		t.Fatal(got)
	}
	for _, request := range requests {
		if !request.TLS || request.Expected != expected {
			t.Fatal(request)
		}
	}
	if rejected.closed.Load() != 2 || winner.closed.Load() != 0 {
		t.Fatal("connection ownership lost")
	}
}

func TestDialIdentityOrPinRefusalNeverFallsBack(t *testing.T) {
	for _, pin := range []bool{false, true} {
		cause := errors.New("peer verifier refused")
		conn := failingConnection(errors.New("unavailable"))
		if pin {
			conn.evidence.PinMismatch = cause
		} else {
			conn.evidence.IdentityMismatch = cause
		}
		calls := 0
		connector := fakeConnector(func(context.Context, platform.DeviceConnectRequest) (platform.DeviceConnection, error) {
			calls++
			return conn, nil
		})
		result := Dial(context.Background(), connector, Target{Addr: "10.0.0.1:50051", Candidates: []string{"10.0.0.1:50051", "10.0.0.2:50051"}}, []Credential{{Reference: "a"}}, DialOptions{})
		if !errors.Is(result.Err, ErrIdentityRefused) || !errors.Is(result.Err, cause) || calls != 1 || conn.closed.Load() != 1 {
			t.Fatalf("result=%+v calls=%d closed=%d", result, calls, conn.closed.Load())
		}
	}
}

func TestDialEagerVerifierRefusalNeverFallsBack(t *testing.T) {
	cause := errors.New("wrong peer")
	calls := 0
	connector := fakeConnector(func(context.Context, platform.DeviceConnectRequest) (platform.DeviceConnection, error) {
		calls++
		return nil, &platform.DevicePeerRejectedError{Cause: cause}
	})
	result := Dial(context.Background(), connector, Target{Addr: "10.0.0.1:50051"}, []Credential{{Reference: "a"}, {Reference: "b"}}, DialOptions{})
	if !errors.Is(result.Err, ErrIdentityRefused) || !errors.Is(result.Err, cause) || calls != 1 {
		t.Fatalf("result=%+v calls=%d", result, calls)
	}
}

func TestDialObservedOrgReordersUntriedCredentials(t *testing.T) {
	var order []string
	winner := workingConnection()
	connector := fakeConnector(func(_ context.Context, request platform.DeviceConnectRequest) (platform.DeviceConnection, error) {
		order = append(order, request.Credential)
		if request.Credential == "org7" {
			return winner, nil
		}
		conn := failingConnection(errors.New("server certificate belongs to org 7"))
		conn.evidence.ObservedOrg = 7
		return conn, nil
	})
	result := Dial(context.Background(), connector, Target{Addr: "10.0.0.1:50051"}, []Credential{{Reference: "org8", OrganizationID: 8}, {Reference: "org9", OrganizationID: 9}, {Reference: "org7", OrganizationID: 7}}, DialOptions{})
	if result.Err != nil || result.LastMTLSError != nil || !reflect.DeepEqual(order, []string{"org8", "org7"}) {
		t.Fatalf("result=%+v order=%v", result, order)
	}
}

func TestDialPinnedUnreachableRetainsPin(t *testing.T) {
	var calls int
	connector := fakeConnector(func(_ context.Context, request platform.DeviceConnectRequest) (platform.DeviceConnection, error) {
		if !request.TLS {
			t.Fatal("plaintext offered for a pinned device")
		}
		calls++
		conn := failingConnection(errors.New("connection refused"))
		conn.evidence.TCPDialError = errors.New("connection refused")
		return conn, nil
	})
	result := Dial(context.Background(), connector, Target{Addr: "10.0.0.1:50051", PinnedKey: "pi"}, []Credential{{Reference: "a"}, {Reference: "b"}}, DialOptions{})
	if result.Failure != PinnedEndpointUnavailable || !errors.Is(result.Err, ErrNoAuthenticatedEndpoint) || calls != 2 {
		t.Fatalf("result=%+v calls=%d", result, calls)
	}
}

func TestDialPrimaryCertificateRejectionOverridesSecondaryReachability(t *testing.T) {
	connector := fakeConnector(func(_ context.Context, request platform.DeviceConnectRequest) (platform.DeviceConnection, error) {
		if !request.TLS {
			t.Fatal("plaintext after primary rejection")
		}
		if request.Address == "10.0.0.1:50051" {
			conn := failingConnection(&platform.RPCStatusError{Code: platform.RPCUnavailable, Message: "remote error: tls: bad certificate"})
			conn.evidence.ObservedOrg = 7
			return conn, nil
		}
		conn := failingConnection(errors.New("connection refused"))
		conn.evidence.TCPDialError = errors.New("connection refused")
		return conn, nil
	})
	result := Dial(context.Background(), connector, Target{Addr: "10.0.0.1:50051", Candidates: []string{"10.0.0.1:50051", "10.0.0.2:50051"}}, []Credential{{Reference: "a"}}, DialOptions{})
	if result.Failure != CertificateRejected || result.ObservedOrg != 7 || !result.AnyCertRejection {
		t.Fatalf("%+v", result)
	}
}

func TestDialPlaintextWaitsForAuthenticatedCandidates(t *testing.T) {
	var mu sync.Mutex
	var requests []platform.DeviceConnectRequest
	plaintext := workingConnection()
	connector := fakeConnector(func(_ context.Context, request platform.DeviceConnectRequest) (platform.DeviceConnection, error) {
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		if !request.TLS {
			return plaintext, nil
		}
		return nil, errors.New("connection refused")
	})
	result := Dial(context.Background(), connector, Target{Addr: "10.0.0.1:50051", Candidates: []string{"10.0.0.1:50051", "10.0.0.2:50051"}}, []Credential{{Reference: "a"}}, DialOptions{})
	if result.Err != nil || result.Connection != plaintext || len(requests) != 5 || requests[4].TLS || requests[4].Address != "10.0.0.1:50051" {
		t.Fatalf("result=%+v requests=%v", result, requests)
	}
}

func TestDialBackupWinsAndClosesLatePrimary(t *testing.T) {
	primary := workingConnection()
	backup := workingConnection()
	connector := fakeConnector(func(ctx context.Context, request platform.DeviceConnectRequest) (platform.DeviceConnection, error) {
		if request.Address == "10.0.0.1:50051" {
			primary.probe = func(ctx context.Context, _ platform.RPCRequest) (platform.RPCResponse, error) {
				<-ctx.Done()
				return platform.RPCResponse{}, nil
			}
			return primary, nil
		}
		return backup, nil
	})
	result := Dial(context.Background(), connector, Target{Addr: "10.0.0.1:50051", Candidates: []string{"10.0.0.1:50051", "10.0.0.2:50051"}}, []Credential{{Reference: "a"}}, DialOptions{})
	if result.Err != nil || result.Connection != backup || primary.closed.Load() != 1 || backup.closed.Load() != 0 {
		t.Fatalf("result=%+v closed=%d,%d", result, primary.closed.Load(), backup.closed.Load())
	}
}

func TestDialCancellationJoinsAndCloses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	conn := workingConnection()
	conn.probe = func(ctx context.Context, _ platform.RPCRequest) (platform.RPCResponse, error) {
		cancel()
		<-ctx.Done()
		return platform.RPCResponse{}, ctx.Err()
	}
	connector := fakeConnector(func(context.Context, platform.DeviceConnectRequest) (platform.DeviceConnection, error) {
		return conn, nil
	})
	result := Dial(ctx, connector, Target{Addr: "10.0.0.1:50051"}, []Credential{{Reference: "a"}}, DialOptions{})
	if !errors.Is(result.Err, context.Canceled) || conn.closed.Load() != 1 {
		t.Fatalf("result=%+v closed=%d", result, conn.closed.Load())
	}
}

func TestAgentVersionWireAndStatus(t *testing.T) {
	conn := workingConnection()
	version, err := GetAgentVersion(context.Background(), conn)
	if err != nil || version.GetVersion() != "test" || version.GetHostname() != "pi" {
		t.Fatalf("%v %v", version, err)
	}
	status := &platform.RPCStatusError{Code: platform.RPCUnauthenticated, Message: "certificate is not valid for client authentication", TrailingMetadata: platform.Metadata{{Name: "test", Value: []byte("value")}}}
	_, err = GetAgentVersion(context.Background(), failingConnection(status))
	if err != status || !isCertRejectionError("pi:50051", err) {
		t.Fatalf("status lost: %v", err)
	}
}

func TestRecoveryAndCredentialExpiry(t *testing.T) {
	now := time.Now()
	credentials := []Credential{{OrganizationID: 7, NotAfter: now.Add(-time.Hour)}, {OrganizationID: 8, NotAfter: now.Add(time.Hour)}}
	if ExpiredClientCertificateReason(7, credentials, now) == "" || ExpiredClientCertificateReason(8, credentials, now) != "" {
		t.Fatal("expiry evidence crossed orgs")
	}
	for _, refused := range []bool{false, true} {
		decision := DecideRecovery(context.Background(), errors.New("TLS failed"), RecoveryEvidence{Refused: refused, ClockSuspect: true, Interactive: true})
		if decision.SyncClock == refused || decision.OfferRefresh {
			t.Fatal(decision)
		}
	}
	decision := DecideRecovery(context.Background(), errors.New("TLS failed"), RecoveryEvidence{ClockSuspect: true, RefreshReason: "expired", Interactive: true, SwitchStep: "switch context"})
	if decision.SyncClock || decision.OfferRefresh {
		t.Fatal(decision)
	}
	if CertificateRefreshReason(errors.New("remote error: tls: expired certificate")) != "" {
		t.Fatal("generic peer alert must not refresh credentials")
	}
}

func TestComparePinRotationAndPersistence(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	previous, persist, err := ComparePin("pi", "pi", []byte("old"), now.Add(time.Hour), nil, now)
	if err != nil || !persist {
		t.Fatal(persist, err)
	}
	_, persist, err = ComparePin("pi", "pi", []byte("old"), now.Add(time.Hour), &previous, now.Add(time.Minute))
	if err != nil || persist {
		t.Fatal("LastSeen-only changes must not write", err)
	}
	_, _, err = ComparePin("pi", "pi", []byte("new"), now.Add(time.Hour), &previous, now)
	var mismatch *PinMismatchError
	if !errors.As(err, &mismatch) || mismatch.Want != SPKIFingerprint([]byte("old")) {
		t.Fatal(err)
	}
	_, persist, err = ComparePin("pi", "pi", []byte("new"), now.Add(2*time.Hour), &previous, now.Add(time.Hour))
	if err != nil || !persist {
		t.Fatal("rotation at expiry must be accepted", err)
	}
}
