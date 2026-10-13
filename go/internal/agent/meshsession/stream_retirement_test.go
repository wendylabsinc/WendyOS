package meshsession

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

type retirementAuthorizer struct {
	port  uint16
	calls atomic.Int32
}

func (a *retirementAuthorizer) DialAuthorized(port uint16, dial func() (net.Conn, error)) (net.Conn, error) {
	a.calls.Add(1)
	if port != a.port {
		return nil, ErrDenied
	}
	return dial()
}

type retirementFixture struct {
	client     *Client
	peer       int32
	endpoint   netip.AddrPort
	port       uint16
	authorizer *retirementAuthorizer
}

func newRetirementFixture(t *testing.T, handler http.Handler) *retirementFixture {
	t.Helper()
	a, b := fixtureCredentials(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	app := &http.Server{Handler: handler}
	go app.Serve(listener)
	t.Cleanup(func() { app.Close() })
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	authorizer := &retirementAuthorizer{port: port}
	server, err := NewServer(b, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, "127.0.0.1:0") }()
	t.Cleanup(func() {
		cancel()
		server.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("server handlers failed to join")
		}
	})
	for server.Addr() == nil {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	client, err := NewClient(a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return &retirementFixture{client: client, peer: b.Asset, endpoint: netip.MustParseAddrPort(server.Addr().String()), port: port, authorizer: authorizer}
}
func (f *retirementFixture) dial(t *testing.T, port uint16) (net.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return f.client.Dial(ctx, f.peer, f.endpoint, port)
}
func retirementHTTP(t *testing.T, flow net.Conn, reader *bufio.Reader) {
	t.Helper()
	flow.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(flow, "GET / HTTP/1.1\r\nHost: retirement-fixture\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "retirement fixture\n" || response.Close {
		t.Fatalf("response %q close=%v error=%v", body, response.Close, err)
	}
}
func retirementHTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "retirement fixture\n") })
}

func TestAppStreamsRetireAcrossTwentySustainedHTTPAndDeniedCycles(t *testing.T) {
	f := newRetirementFixture(t, retirementHTTPHandler())
	held, err := f.dial(t, f.port)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	reader := bufio.NewReader(held)
	original := held.(*streamConn).conn
	denied := uint16(1)
	if f.port == denied {
		denied = 2
	}
	for i := 0; i < 20; i++ {
		retirementHTTP(t, held, reader)
		next, err := f.dial(t, f.port)
		if err != nil {
			t.Fatalf("cycle %d allowed: %v; original context=%v", i, err, original.Context().Err())
		}
		if next.(*streamConn).conn != original {
			t.Fatal("new flow silently replaced shared QUIC connection")
		}
		retirementHTTP(t, next, bufio.NewReader(next))
		next.Close()
		if rejected, err := f.dial(t, denied); !errors.Is(err, ErrDenied) || rejected != nil {
			t.Fatalf("cycle %d denial ACK lost: flow=%v error=%v", i, rejected, err)
		}
		if original.Context().Err() != nil {
			t.Fatal("denial closed held shared session")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if f.authorizer.calls.Load() != 41 {
		t.Fatalf("unexpected authorization/retry count %d", f.authorizer.calls.Load())
	}
	retirementHTTP(t, held, reader)
}

func TestRejectedAppStreamsReturnCreditWithoutClosingSharedSession(t *testing.T) {
	for _, kind := range []string{"denied", "invalid-magic", "unsupported-datagram", "short-header", "read-reset"} {
		t.Run(kind, func(t *testing.T) {
			f := newRetirementFixture(t, retirementHTTPHandler())
			held, err := f.dial(t, f.port)
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			original := held.(*streamConn).conn
			reader := bufio.NewReader(held)
			for i := 0; i < 24; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				stream, err := original.OpenStreamSync(ctx)
				cancel()
				if err != nil {
					t.Fatalf("%s %d: leaked incoming credit: %v", kind, i, err)
				}
				stream.SetDeadline(time.Now().Add(2 * time.Second))
				hello := []byte{'W', 'A', 'S', '1', 0, 0}
				binary.BigEndian.PutUint16(hello[4:], f.port)
				switch kind {
				case "denied":
					binary.BigEndian.PutUint16(hello[4:], 1)
					if f.port == 1 {
						binary.BigEndian.PutUint16(hello[4:], 2)
					}
				case "invalid-magic":
					copy(hello, "BAD!")
				case "unsupported-datagram":
					copy(hello, "WAU1")
				case "short-header", "read-reset":
					hello = hello[:3]
				}
				if _, err := stream.Write(hello); err != nil {
					t.Fatal(err)
				}
				if kind == "read-reset" {
					stream.CancelWrite(7)
				} else if kind != "denied" {
					stream.Close()
				}
				data, err := io.ReadAll(stream)
				// The rejected send direction is gracefully closed, so its exact ACK is
				// delivered even though the peer's unused request direction is canceled.
				if err != nil {
					t.Fatalf("%s %d did not finish rejected response: %v", kind, i, err)
				}
				if kind == "denied" && !bytes.Equal(data, []byte{0}) {
					t.Fatalf("denial ACK=%v", data)
				}
				if kind != "denied" && len(data) != 0 {
					t.Fatalf("malformed request received data: %v", data)
				}
				stream.CancelRead(0)
				stream.Close()
				retirementHTTP(t, held, reader)
				if original.Context().Err() != nil {
					t.Fatal("bad stream closed unrelated held session")
				}
			}
			expected := int32(1)
			if kind == "denied" {
				expected += 24
			}
			if f.authorizer.calls.Load() != expected {
				t.Fatalf("malformed stream reached authorizer: %d", f.authorizer.calls.Load())
			}
		})
	}
}

func TestRejectedStreamsDoNotOccupyAllServerHandlers(t *testing.T) {
	f := newRetirementFixture(t, retirementHTTPHandler())
	held, err := f.dial(t, f.port)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	q := held.(*streamConn).conn
	// One live flow plus seven rejected flows fills the original advertised
	// window. Every rejection must return credit and its server handler slot.
	for batch := 0; batch < 3; batch++ {
		for i := 0; i < 7; i++ {
			port := uint16(1)
			if f.port == 1 {
				port = 2
			}
			if _, err := f.dial(t, port); !errors.Is(err, ErrDenied) {
				t.Fatal(err)
			}
		}
		flows := []net.Conn{held}
		for i := 0; i < 7; i++ {
			flow, err := f.dial(t, f.port)
			if err != nil {
				t.Fatalf("batch %d admitted handler %d: %v", batch, i, err)
			}
			flows = append(flows, flow)
			if flow.(*streamConn).conn != q {
				t.Fatal("handler exhaustion hid behind another QUIC connection")
			}
		}
		for _, flow := range flows[1:] {
			retirementHTTP(t, flow, bufio.NewReader(flow))
			flow.Close()
		}
		retirementHTTP(t, held, bufio.NewReader(held))
	}
}
