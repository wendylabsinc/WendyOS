package providers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/proto"
)

func readConsoleTestMessage(conn net.Conn) (*pb.WendyComMessage, error) {
	var header [8]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	if header[0] != 0xa5 || header[1] != 2 {
		return nil, fmt.Errorf("unexpected WendyCom header: %x", header)
	}
	body := make([]byte, binary.BigEndian.Uint16(header[6:]))
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, err
	}
	message := new(pb.WendyComMessage)
	return message, proto.Unmarshal(body, message)
}

func writeConsoleTestMessage(conn net.Conn, message *pb.WendyComMessage) error {
	body, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	header := []byte{0xa5, 2, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(header[6:], uint16(len(body)))
	_, err = conn.Write(append(header, body...))
	return err
}

func TestStreamConsoleCancelsUnansweredCommands(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []struct {
		name    string
		request int
	}{
		{name: "attach", request: 1},
		{name: "renewal", request: 2},
	} {
		t.Run(stage.name, func(t *testing.T) {
			listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
				Certificates: []tls.Certificate{{Certificate: [][]byte{certificate}, PrivateKey: private}},
				MinVersion:   tls.VersionTLS12,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reached := make(chan struct{})
			serverDone := make(chan error, 1)
			go func() {
				serverDone <- func() error {
					conn, err := listener.Accept()
					if err != nil {
						return err
					}
					defer conn.Close()
					if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
						return err
					}
					handshake, err := readConsoleTestMessage(conn)
					if err != nil {
						return err
					}
					if handshake.GetHandshake() == nil {
						return fmt.Errorf("expected WendyCom handshake")
					}
					if err := writeConsoleTestMessage(conn, handshake); err != nil {
						return err
					}
					for request := 1; request <= stage.request; request++ {
						message, err := readConsoleTestMessage(conn)
						if err != nil {
							return err
						}
						command := message.GetCommand()
						if command.GetConsoleAttach() == nil {
							return fmt.Errorf("expected console attach, got %v", message)
						}
						if request == stage.request {
							// Do not answer this attach or renewal. Cancellation
							// must close the connection and wake its pending waiter.
							close(reached)
							_, err = readConsoleTestMessage(conn)
							if err == nil {
								return fmt.Errorf("received a command before the pending request was interrupted")
							}
							if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
								return fmt.Errorf("client left the console connection open: %w", err)
							}
							return nil
						}
						if err := writeConsoleTestMessage(conn, &pb.WendyComMessage{
							Msg: &pb.WendyComMessage_Response{Response: &pb.WendyComResponse{RequestId: command.RequestId}},
						}); err != nil {
							return err
						}
					}
					return nil
				}()
			}()
			host, port, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			completed := make(chan error, 1)
			go func() {
				completed <- (&MicroWendyProvider{}).StreamConsole(ctx, models.ExternalDevice{
					ConnectionInfo: map[string]string{"type": "LAN", "ip": host, "port": port},
				}, func(liteclient.ConsoleChunk) error { return nil })
			}()
			select {
			case <-reached:
			case err := <-serverDone:
				t.Fatalf("fake console failed before %s: %v", stage.name, err)
			case err := <-completed:
				t.Fatalf("console stopped before %s: %v", stage.name, err)
			case <-time.After(15 * time.Second):
				t.Fatalf("console never reached %s", stage.name)
			}
			cancel()
			select {
			case err := <-completed:
				if err != nil {
					t.Fatalf("cancelled console: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatalf("cancellation did not interrupt unanswered %s", stage.name)
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}
