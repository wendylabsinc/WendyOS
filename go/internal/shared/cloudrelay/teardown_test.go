package cloudrelay

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestRelayTCPTeardown(t *testing.T) {
	const request = "request followed by EOF"
	const response = "response after request EOF"
	for _, tt := range []struct {
		name        string
		respond     bool
		halfClose   bool
		wantRequest string
	}{
		{name: "response after FIN", respond: true, halfClose: true, wantRequest: request},
		{name: "disconnect after FIN", halfClose: true, wantRequest: request},
		{name: "full close"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			type result struct {
				data string
				err  error
			}
			requestRead := make(chan result, 1)
			serviceReady := make(chan struct{})
			keepServiceOpen := make(chan struct{})
			var callerClosed <-chan struct{}
			testAuthorizedRelay(t, "tcp", false, &relayLifecycle{
				tcpHalfClose: true,
				configure: func(r *testRelay, _ *Agent) {
					r.callerClosed = make(chan struct{})
					r.agentClosed = make(chan struct{})
					callerClosed = r.callerClosed
				},
				tcpService: func(conn net.Conn) {
					close(serviceReady)
					received, err := io.ReadAll(conn)
					requestRead <- result{data: string(received), err: err}
					if tt.respond {
						_, _ = io.WriteString(conn, response)
					} else {
						<-keepServiceOpen
					}
				},
				tcpExercise: func(conn net.Conn, agent *Agent) {
					defer close(keepServiceOpen)
					awaitRelayPhase(t, serviceReady, "agent did not dial the local TCP service")
					if len(agent.slots) != 1 {
						t.Fatal("admitted TCP relay is missing its session slot")
					}
					if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
						t.Fatal(err)
					}
					if tt.halfClose {
						if _, err := io.WriteString(conn, request); err != nil {
							t.Fatal(err)
						}
						half, ok := conn.(interface{ CloseWrite() error })
						if !ok {
							t.Fatal("Cloud relay connection does not support CloseWrite")
						}
						if err := half.CloseWrite(); err != nil {
							t.Fatal(err)
						}
					} else if err := conn.Close(); err != nil {
						t.Fatal(err)
					}
					select {
					case got := <-requestRead:
						if got.err != nil || got.data != tt.wantRequest {
							t.Fatalf("service got %q, %v, want %q and clean EOF", got.data, got.err, tt.wantRequest)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("device service did not observe EOF")
					}
					if tt.respond {
						got, err := io.ReadAll(conn)
						if err != nil || string(got) != response {
							t.Fatalf("caller got %q, %v, want %q and clean EOF", got, err, response)
						}
					} else if tt.halfClose {
						if err := conn.Close(); err != nil {
							t.Fatal(err)
						}
					}
					awaitRelayPhase(t, callerClosed, "caller join did not close")
					deadline := time.Now().Add(3 * time.Second)
					for len(agent.slots) != 0 {
						if time.Now().After(deadline) {
							t.Fatal("agent relay slot still in use after caller disconnected")
						}
						time.Sleep(time.Millisecond)
					}
				},
			})
		})
	}
}

func TestRelayTCPFramedResponseCompatibility(t *testing.T) {
	const request = "GET /snapshot HTTP/1.1\r\nHost: example\r\nConnection: close\r\n\r\n"
	const response = "HTTP/1.1 200 OK\r\nContent-Length: 4\r\nConnection: close\r\n\r\ndata"
	for _, tt := range []struct {
		name      string
		halfClose bool
	}{
		{name: "unknown peer"},
		{name: "capable peer", halfClose: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			completeRequest := make(chan struct{})
			reply := make(chan struct{})
			testAuthorizedRelay(t, "tcp", false, &relayLifecycle{
				tcpHalfClose: tt.halfClose,
				configure: func(r *testRelay, _ *Agent) {
					r.callerClosed = make(chan struct{})
					r.agentClosed = make(chan struct{})
				},
				tcpService: func(conn net.Conn) {
					buf := make([]byte, len(request))
					if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != request {
						return
					}
					close(completeRequest)
					<-reply
					if tt.halfClose {
						if extra, err := io.ReadAll(conn); err != nil || len(extra) != 0 {
							return
						}
					}
					_, _ = io.WriteString(conn, response)
				},
				tcpExercise: func(conn net.Conn, _ *Agent) {
					defer close(reply)
					half, hasHalfClose := conn.(interface{ CloseWrite() error })
					if hasHalfClose != tt.halfClose {
						t.Fatalf("CloseWrite support is %t, want %t", hasHalfClose, tt.halfClose)
					}
					if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
						t.Fatal(err)
					}
					if _, err := io.WriteString(conn, request); err != nil {
						t.Fatal(err)
					}
					if hasHalfClose {
						if err := half.CloseWrite(); err != nil {
							t.Fatal(err)
						}
					}
					awaitRelayPhase(t, completeRequest, "framed request did not reach the device")
					reply <- struct{}{}
					got, err := io.ReadAll(conn)
					if err != nil || string(got) != response {
						t.Fatalf("caller got %q, %v, want the complete framed response", got, err)
					}
				},
			})
		})
	}
}
