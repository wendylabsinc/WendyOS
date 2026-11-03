package cloudrelay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/credentials"
)

func TestUplinkPrimarySuccessSkipsAlternatives(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	d := &uplinkDialer{timeout: time.Second, candidates: func(context.Context) ([]uplink, error) {
		t.Fatal("enumerated interfaces despite healthy primary")
		return nil, nil
	}}
	conn, err := d.connect(context.Background(), func(context.Context, uplink) (net.Conn, error) { return left, nil })
	if err != nil || conn != left {
		t.Fatalf("primary: %v, %v", conn, err)
	}
}

func TestUplinkBlackholeFallsBackAndClosesLosers(t *testing.T) {
	primaryStopped := make(chan struct{})
	loserStarted := make(chan struct{})
	left, right := net.Pipe()
	defer right.Close()
	loser, loserPeer := net.Pipe()
	defer loserPeer.Close()
	d := &uplinkDialer{primary: uplink{name: "primary"}, timeout: 20 * time.Millisecond,
		candidates: func(context.Context) ([]uplink, error) { return []uplink{{name: "loser"}, {name: "wifi"}}, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := d.connect(ctx, func(ctx context.Context, path uplink) (net.Conn, error) {
		switch path.name {
		case "primary":
			<-ctx.Done()
			close(primaryStopped)
			return nil, ctx.Err()
		case "loser":
			close(loserStarted)
			<-ctx.Done()
			// A dial can finish at the same time cancellation arrives.
			return loser, nil
		default:
			<-primaryStopped
			<-loserStarted
			return left, nil
		}
	})
	if err != nil || conn != left {
		t.Fatalf("fallback: %v, %v", conn, err)
	}
	defer conn.Close()
	if _, err := loserPeer.Write([]byte("closed")); err == nil {
		t.Fatal("losing connection leaked")
	}
}

func TestUplinkCancellationAndFailures(t *testing.T) {
	primaryErr, wifiErr := errors.New("ethernet down"), errors.New("wifi down")
	d := &uplinkDialer{primary: uplink{name: "primary"}, timeout: time.Second,
		candidates: func(context.Context) ([]uplink, error) { return []uplink{{name: "wifi"}}, nil }}
	_, err := d.connect(context.Background(), func(_ context.Context, path uplink) (net.Conn, error) {
		if path.name == "primary" {
			return nil, primaryErr
		}
		return nil, wifiErr
	})
	if !errors.Is(err, primaryErr) || !errors.Is(err, wifiErr) {
		t.Fatalf("lost dial errors: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err = d.connect(ctx, func(ctx context.Context, path uplink) (net.Conn, error) {
		if path.name == "primary" {
			return nil, primaryErr
		}
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestUplinkTLSFailureTriesAuthenticatedAlternative(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	creds := credentials.NewTLS(&tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS13})
	for _, wrongName := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "wrong hostname"}[wrongName], func(t *testing.T) {
			c := creds.Clone()
			address := "example.com:443"
			if wrongName {
				address = "wrong.example:443"
			}
			var attempted atomic.Bool
			d := &uplinkDialer{timeout: time.Second,
				primary: uplink{dial: func(context.Context, string, string) (net.Conn, error) {
					left, right := net.Pipe()
					right.Close() // TCP succeeds but TLS cannot complete.
					return left, nil
				}},
				candidates: func(context.Context) ([]uplink, error) {
					return []uplink{{name: "wifi", dial: func(ctx context.Context, network, address string) (net.Conn, error) {
						attempted.Store(true)
						if address != "example.com:443" && address != "wrong.example:443" {
							t.Errorf("hostname lost before fallback: %s", address)
						}
						return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
					}}}, nil
				}}
			conn, err := d.dialAuthenticated(context.Background(), address, c)
			if !attempted.Load() {
				t.Fatal("TLS failure did not try Wi-Fi")
			}
			if wrongName {
				if err == nil {
					conn.Close()
					t.Fatal("accepted invalid TLS identity on fallback")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_, auth, err := (uplinkCredentials{c}).ClientHandshake(context.Background(), "example.com:443", conn)
			if err != nil {
				t.Fatal(err)
			}
			info, ok := auth.(credentials.TLSInfo)
			if !ok || info.SecurityLevel != credentials.PrivacyAndIntegrity || len(info.State.VerifiedChains) == 0 {
				t.Fatalf("lost verified TLS identity: %+v", auth)
			}
		})
	}
}
