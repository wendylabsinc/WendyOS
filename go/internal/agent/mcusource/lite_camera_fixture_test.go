package mcusource

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math/big"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// This synthetic source implements the actual TLS/WendyCom wire boundary. It
// is not a recording from physical Lite firmware. The transport, supervisor,
// kernel writer and consumer under test are the production implementations.
type liteCameraFixture struct {
	address, certPEM, chainPEM, keyPEM string
	pairing                            SensorPairing
	phase                              atomic.Int32
	accepted                           atomic.Int32
	verifiedConsumer                   atomic.Bool
}

func newLiteCameraFixture(t *testing.T, ctx context.Context) *liteCameraFixture {
	t.Helper()
	const base = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/"
	fixture := &liteCameraFixture{pairing: SensorPairing{SourcePrincipalURI: base + "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Transport: "wendycom", Name: "synthetic-lite-camera"}}
	root, rootKey := liteCameraCertificate(t, 1, "", nil, nil)
	consumer, consumerKey := liteCameraCertificate(t, 2, base+"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", root, rootKey)
	source, sourceKey := liteCameraCertificate(t, 3, fixture.pairing.SourcePrincipalURI, root, rootKey)
	fixture.certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: consumer.Raw}))
	fixture.chainPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	keyDER, err := x509.MarshalECPrivateKey(consumerKey)
	if err != nil {
		t.Fatal(err)
	}
	fixture.keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	pool := x509.NewCertPool()
	pool.AddCert(root)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture.address = listener.Addr().String()
	frames := [2][]byte{liteCameraJPEG(t, 0), liteCameraJPEG(t, 1)}
	serverCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, err := listener.Accept()
		if err != nil {
			return
		}
		fixture.accepted.Add(1)
		defer raw.Close()
		closeOnStop := context.AfterFunc(serverCtx, func() { _ = raw.Close() })
		defer closeOnStop()
		// A real TLS flight outlasts the old 3-second manifest budget. The
		// supervisor must finish Connect before starting FetchManifest's clock.
		select {
		case <-time.After(3200 * time.Millisecond):
		case <-serverCtx.Done():
			return
		}
		secure := tls.Server(raw, &tls.Config{MinVersion: tls.VersionTLS13,
			Certificates: []tls.Certificate{{Certificate: [][]byte{source.Raw}, PrivateKey: sourceKey}},
			ClientAuth:   tls.RequireAndVerifyClientCert, ClientCAs: pool, NextProtos: []string{"wendy-com/2"}})
		if err := secure.HandshakeContext(serverCtx); err != nil {
			return
		}
		fixture.verifiedConsumer.Store(secure.ConnectionState().PeerCertificates[0].Equal(consumer))
		var writeMu sync.Mutex
		send := func(message *litepb.WendyComMessage) error {
			body, err := proto.Marshal(message)
			if err != nil {
				return err
			}
			packet := make([]byte, 8+len(body))
			packet[0], packet[1] = 0xa5, 2
			binary.BigEndian.PutUint16(packet[6:8], uint16(len(body)))
			copy(packet[8:], body)
			writeMu.Lock()
			defer writeMu.Unlock()
			_ = secure.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_, err = secure.Write(packet)
			return err
		}
		var subscribed atomic.Bool
		publisherDone := make(chan struct{})
		publisherCtx, stopPublisher := context.WithCancel(serverCtx)
		defer func() { stopPublisher(); <-publisherDone }()
		go func() {
			defer close(publisherDone)
			ticker := time.NewTicker(40 * time.Millisecond)
			defer ticker.Stop()
			var sequence uint32
			for {
				select {
				case <-publisherCtx.Done():
					return
				case <-ticker.C:
					if !subscribed.Load() {
						continue
					}
					sequence++
					if err := send(&litepb.WendyComMessage{Msg: &litepb.WendyComMessage_SensorData{SensorData: &sensorlinkpb.SensorData{
						ChannelId: 1, FrameSeq: sequence, TsUs: uint64(time.Now().UnixMicro()),
						Flags: sensorlink.FlagKeyframe | sensorlink.FlagLastChunk, Payload: frames[fixture.phase.Load()],
					}}}); err != nil {
						return
					}
				}
			}
		}()
		for {
			message, err := readLiteCameraMessage(secure)
			if err != nil {
				return
			}
			if message.GetHandshake() != nil {
				if send(message) != nil {
					return
				}
				continue
			}
			command := message.GetCommand()
			if command == nil {
				return
			}
			response := &litepb.WendyComResponse{RequestId: command.RequestId}
			switch {
			case command.GetSensorLinkGetManifest() != nil:
				response.Data = &litepb.WendyComResponse_SensorLinkManifest{SensorLinkManifest: &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{{
					ChannelId: 1, InputId: 1, Name: "camera",
					Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG, Width: 64, Height: 48, Fps: 25}},
				}}}}
			case command.GetSensorLinkSubscribe() != nil:
				channels := command.GetSensorLinkSubscribe().GetChannelId()
				if len(channels) != 1 || channels[0] != 1 {
					return
				}
				subscribed.Store(true)
			case command.GetSensorLinkUnsubscribe() != nil:
				subscribed.Store(false)
			default:
				return
			}
			if send(&litepb.WendyComMessage{Msg: &litepb.WendyComMessage_Response{Response: response}}) != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		stop()
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("synthetic Lite TLS source did not shut down")
		}
	})
	return fixture
}

func (f *liteCameraFixture) transport(p SensorPairing, address string) (SensorTransport, error) {
	return NewWendyComTransport(zap.NewNop(), f.certPEM, f.chainPEM, f.keyPEM, p, address)
}

func readLiteCameraMessage(reader io.Reader) (*litepb.WendyComMessage, error) {
	var header [8]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	if header[0] != 0xa5 || header[1] != 2 || header[2] != 0 || header[3] != 0 {
		return nil, fmt.Errorf("unexpected WendyCom header %x", header)
	}
	body := make([]byte, int(binary.BigEndian.Uint16(header[6:8])))
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	message := &litepb.WendyComMessage{}
	return message, proto.Unmarshal(body, message)
}

func liteCameraCertificate(t *testing.T, serial int64, uri string, parent *x509.Certificate, signer *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "synthetic-camera-fixture"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true,
		IsCA: parent == nil, KeyUsage: x509.KeyUsageDigitalSignature}
	if parent == nil {
		template.KeyUsage |= x509.KeyUsageCertSign
		parent, signer = template, key
	} else {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}
		parsed, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		template.URIs = []*url.URL{parsed}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
}

func liteCameraJPEG(t *testing.T, phase int32) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := range 48 {
		for x := range 64 {
			pixel := color.RGBA{R: 240, A: 255}
			if phase == 1 {
				pixel = color.RGBA{G: 240, A: 255}
			}
			if x >= 32 {
				pixel = color.RGBA{B: 240, A: 255}
			}
			picture.SetRGBA(x, y, pixel)
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, picture, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestLiteCameraFixtureAuthenticatesAndDeliversChangingPixels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := newLiteCameraFixture(t, ctx)
	transport, err := fixture.transport(fixture.pairing, fixture.address)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	if err := transport.(interface{ Connect(context.Context) error }).Connect(ctx); err != nil {
		t.Fatal(err)
	}
	manifest, err := transport.FetchManifest(ctx)
	if err != nil || len(manifest.GetSensors()) != 1 {
		t.Fatalf("manifest: %+v, %v", manifest, err)
	}
	frames, closeStream, err := transport.Stream(ctx, []uint32{1})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStream()
	for _, phase := range []int32{0, 1} {
		fixture.phase.Store(phase)
		for {
			select {
			case frame, ok := <-frames:
				if !ok {
					t.Fatal("authenticated fixture frame stream closed")
				}
				picture, err := jpeg.Decode(bytes.NewReader(frame.Payload))
				if err != nil {
					t.Fatal(err)
				}
				red, green, _, _ := picture.At(16, 24).RGBA()
				if (phase == 0 && red > 200*257 && green < 45*257) || (phase == 1 && green > 200*257 && red < 45*257) {
					goto observed
				}
			case <-ctx.Done():
				t.Fatal("authenticated fixture did not deliver changed JPEG pixels")
			}
		}
	observed:
	}
	if !fixture.verifiedConsumer.Load() || fixture.accepted.Load() != 1 {
		t.Fatal("manifest and video did not use one mutually authenticated connection")
	}
}
