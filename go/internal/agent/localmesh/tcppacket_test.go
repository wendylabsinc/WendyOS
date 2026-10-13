package localmesh

import (
	"context"
	"net"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

func TestQUICOverConfiguredTCPPair(t *testing.T) {
	a, b := testCredentials(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := make(chan error, 1)
	release := make(chan struct{})
	defer close(release)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			server <- err
			return
		}
		pc, err := NewTCPPacketConn(c)
		if err != nil {
			c.Close()
			server <- err
			return
		}
		transport := &quic.Transport{Conn: pc}
		defer transport.Close()
		tlsConfig, err := b.PeerTLS(a.Asset)
		if err != nil {
			server <- err
			return
		}
		ql, err := transport.Listen(tlsConfig, QUICConfig())
		if err != nil {
			server <- err
			return
		}
		defer ql.Close()
		conn, err := ql.Accept(ctx)
		if err != nil {
			server <- err
			return
		}
		defer conn.CloseWithError(0, "test done")
		stream, err := OpenControl(ctx, conn, b.Org, b.Asset, a.Asset)
		if err != nil {
			server <- err
			return
		}
		server <- stream.Close()
		<-release
	}()
	c, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	pc, err := NewTCPPacketConn(c)
	if err != nil {
		t.Fatal(err)
	}
	transport := &quic.Transport{Conn: pc}
	defer transport.Close()
	tlsConfig, err := a.PeerTLS(b.Asset)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := transport.Dial(ctx, pc.RemoteAddr(), tlsConfig, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "test done")
	if _, err = OpenControl(ctx, conn, a.Org, a.Asset, b.Asset); err != nil {
		t.Fatal(err)
	}
	if err = <-server; err != nil {
		t.Fatal(err)
	}
}
