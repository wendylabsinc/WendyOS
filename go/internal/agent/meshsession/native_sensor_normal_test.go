package meshsession

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	sensorpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// The routed native endpoint is an ordinary TCP stream. After its one mTLS
// handshake, the real SensorLink consumer must send WendyCom framing directly,
// without a WAS1 request or another TLS handshake. This fixture speaks the
// source protocol rather than mocking the consumer's connection implementation.
func TestNormalNativeSensorSourceUsesOneTLSConnection(t *testing.T) {
	a, b, _, chain := fixtureCredentialExpiryWithChain(t, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	server, err := NewServer(b, allowPort(func(uint16) bool { return false }))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	config := server.TLSConfig()
	config.NextProtos = nil // native WendyCom has its own protocol exchange
	listener, err := tls.Listen("tcp4", "127.0.0.1:0", config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	want := bytes.Repeat([]byte("native SensorLink frame"), 200)
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		for {
			request, err := readNativeSensorMessage(conn)
			if err != nil {
				done <- err
				return
			}
			if request.GetHandshake() != nil {
				err = writeNativeSensorMessage(conn, request)
			} else if cmd := request.GetCommand(); cmd != nil {
				response := &wendypb.WendyComResponse{RequestId: cmd.RequestId}
				if cmd.GetSensorLinkGetManifest() != nil {
					response.Data = &wendypb.WendyComResponse_SensorLinkManifest{SensorLinkManifest: &sensorpb.SensorManifest{
						Sensors: []*sensorpb.SensorDescriptor{{ChannelId: 1, Name: "camera"}},
					}}
				}
				err = writeNativeSensorMessage(conn, &wendypb.WendyComMessage{Msg: &wendypb.WendyComMessage_Response{Response: response}})
				if err == nil && cmd.GetSensorLinkSubscribe() != nil {
					for index, part := range [][]byte{want[:1024], want[1024:]} {
						data := &sensorpb.SensorData{ChannelId: 1, FrameSeq: 7, ChunkSeq: uint32(index), TsUs: 99, Payload: part}
						if index == 1 {
							data.Flags = sensorlink.FlagLastChunk
						}
						if err = writeNativeSensorMessage(conn, &wendypb.WendyComMessage{Msg: &wendypb.WendyComMessage_SensorData{SensorData: data}}); err != nil {
							break
						}
					}
				}
				if err == nil && cmd.GetSensorLinkUnsubscribe() != nil {
					done <- nil
					return
				}
			} else {
				err = fmt.Errorf("unexpected native source request")
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	key, err := x509.MarshalPKCS8PrivateKey(a.Certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := mcusource.NewWendyComTransport(zap.NewNop(),
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.Certificate.Certificate[0]})), chain,
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})),
		mcusource.SensorPairing{SourcePrincipalURI: b.Device.Principal(), Transport: "wendycom"}, listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	manifest, err := transport.FetchManifest(ctx)
	if err != nil || len(manifest.GetSensors()) != 1 {
		t.Fatalf("native manifest: %v %v", manifest, err)
	}
	frames, closeStream, err := transport.Stream(ctx, []uint32{1})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStream()
	select {
	case frame := <-frames:
		if frame == nil || frame.ChannelID != 1 || frame.Seq != 7 || !bytes.Equal(frame.Payload, want) {
			t.Fatalf("native frame: %+v", frame)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := closeStream(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func readNativeSensorMessage(conn net.Conn) (*wendypb.WendyComMessage, error) {
	var header [8]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	if header[0] != 0xa5 || header[1] != 2 {
		return nil, fmt.Errorf("expected direct WendyCom framing after native mTLS, got %x", header)
	}
	payload := make([]byte, int(binary.BigEndian.Uint16(header[6:])))
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, err
	}
	message := new(wendypb.WendyComMessage)
	return message, proto.Unmarshal(payload, message)
}

func writeNativeSensorMessage(conn net.Conn, message *wendypb.WendyComMessage) error {
	payload, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	header := [8]byte{0xa5, 2}
	binary.BigEndian.PutUint16(header[6:], uint16(len(payload)))
	// Exercise the consumer with ordinary partial stream I/O.
	wire := append(header[:], payload...)
	for len(wire) != 0 {
		n, err := conn.Write(wire[:min(257, len(wire))])
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		wire = wire[n:]
	}
	return nil
}
