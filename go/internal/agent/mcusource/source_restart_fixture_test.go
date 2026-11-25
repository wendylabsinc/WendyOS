package mcusource

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"image/jpeg"
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Synthetic agent-hosted source. The certificate and principal survive a real
// server restart; the address, manifest order, decoded pixels and PCM change.
// This exercises production TLS/gRPC framing, not a physical macOS capture driver.
type restartSourceFixture struct {
	pairing                   SensorPairing
	certPEM, chainPEM, keyPEM string
	tlsConfig                 *tls.Config
	jpeg                      [2][]byte
	verified                  atomic.Int32
}

type restartSensorServer struct {
	agentpbv2.UnimplementedWendySensorServiceServer
	fixture    *restartSourceFixture
	phase      int32
	subscribed atomic.Int32
}

func newRestartSourceFixture(t *testing.T) *restartSourceFixture {
	t.Helper()
	const base = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/"
	f := &restartSourceFixture{pairing: SensorPairing{SourcePrincipalURI: base + "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Transport: "grpc", Name: "synthetic-restart"}}
	root, rootKey := liteCameraCertificate(t, 1, "", nil, nil)
	consumer, consumerKey := liteCameraCertificate(t, 2, base+"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", root, rootKey)
	source, sourceKey := liteCameraCertificate(t, 3, f.pairing.SourcePrincipalURI, root, rootKey)
	f.certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: consumer.Raw}))
	f.chainPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	key, err := x509.MarshalECPrivateKey(consumerKey)
	if err != nil {
		t.Fatal(err)
	}
	f.keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: key}))
	pool := x509.NewCertPool()
	pool.AddCert(root)
	f.tlsConfig = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{source.Raw}, PrivateKey: sourceKey}}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 || !state.PeerCertificates[0].Equal(consumer) {
				return fmt.Errorf("unexpected consumer certificate")
			}
			f.verified.Add(1)
			return nil
		}}
	f.jpeg = [2][]byte{liteCameraJPEG(t, 0), liteCameraJPEG(t, 1)}
	return f
}

func (f *restartSourceFixture) transport(p SensorPairing, address string) (SensorTransport, error) {
	return NewGRPCTransport(zap.NewNop(), f.certPEM, f.chainPEM, f.keyPEM, p, address)
}

func (f *restartSourceFixture) start(t *testing.T, phase int32) (string, *restartSensorServer, func()) {
	t.Helper()
	host := "127.0.0.1"
	// Linux routes all of 127/8 locally without configuring an interface. macOS
	// proves a changed endpoint port; no developer network settings are changed.
	if phase == 1 && runtime.GOOS == "linux" {
		host = "127.0.0.2"
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(f.tlsConfig.Clone())))
	service := &restartSensorServer{fixture: f, phase: phase}
	agentpbv2.RegisterWendySensorServiceServer(server, service)
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	stop := func() {
		server.Stop()
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("source gRPC server failed to stop")
		}
	}
	t.Cleanup(stop)
	return listener.Addr().String(), service, stop
}

func (s *restartSensorServer) GetSensorManifest(context.Context, *agentpbv2.GetSensorManifestRequest) (*sensorlinkpb.SensorManifest, error) {
	camera := &sensorlinkpb.SensorDescriptor{ChannelId: 1, Name: "camera", Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG, Width: 64, Height: 48, Fps: 25}}}
	mic := &sensorlinkpb.SensorDescriptor{ChannelId: 2, Name: "microphone", Format: &sensorlinkpb.SensorDescriptor_Audio{Audio: &sensorlinkpb.AudioFormat{Codec: sensorlinkpb.AudioFormat_PCM_S16LE, SampleRate: 48000, Channels: 1}}}
	sensors := []*sensorlinkpb.SensorDescriptor{camera, mic}
	if s.phase == 1 {
		sensors = []*sensorlinkpb.SensorDescriptor{mic, camera}
	}
	return &sensorlinkpb.SensorManifest{Sensors: sensors}, nil
}

func (s *restartSensorServer) StreamSensors(req *agentpbv2.StreamSensorsRequest, stream agentpbv2.WendySensorService_StreamSensorsServer) error {
	channels := req.GetChannelId()
	if len(channels) != 2 || !((channels[0] == 1 && channels[1] == 2) || (channels[0] == 2 && channels[1] == 1)) {
		return fmt.Errorf("both camera and microphone must be subscribed: %v", channels)
	}
	s.subscribed.Add(1)
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	payloads := [][]byte{s.fixture.jpeg[s.phase], bytes.Repeat(restartPCMMarker(s.phase), 2)}
	var sequence uint32
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-ticker.C:
			sequence++
			for index, payload := range payloads {
				// Two chunks force the real assembler to reconstruct each media frame.
				middle := len(payload) / 2
				timestamp := uint64(time.Now().UnixMicro())
				for part, body := range [][]byte{payload[:middle], payload[middle:]} {
					flags := uint32(0)
					if part == 1 {
						flags = sensorlink.FlagLastChunk
					}
					if index == 0 {
						flags |= sensorlink.FlagKeyframe
					}
					if err := stream.Send(&sensorlinkpb.SensorData{ChannelId: uint32(index + 1), FrameSeq: sequence, ChunkSeq: uint32(part), TsUs: timestamp, Flags: flags, Payload: body}); err != nil {
						return err
					}
				}
			}
		}
	}
}

func restartPCMMarker(phase int32) []byte {
	seed := uint32(0x12345678)
	if phase == 1 {
		seed = 0x87654321
	}
	marker := make([]byte, 960*2)
	for i := 0; i < len(marker); i += 2 {
		seed ^= seed << 13
		seed ^= seed >> 17
		seed ^= seed << 5
		binary.LittleEndian.PutUint16(marker[i:], uint16(seed))
	}
	return marker
}

func assertRestartJPEG(t *testing.T, payload []byte, phase int32) {
	t.Helper()
	picture, err := jpeg.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if picture.Bounds().Dx() != 64 || picture.Bounds().Dy() != 48 {
		t.Fatalf("decoded dimensions: %v", picture.Bounds())
	}
	for _, point := range []struct{ x, channel int }{{16, int(phase)}, {48, 2}} {
		r, g, b, _ := picture.At(point.x, 24).RGBA()
		for channel, value := range []uint32{r >> 8, g >> 8, b >> 8} {
			if (channel == point.channel && value < 200) || (channel != point.channel && value > 45) {
				t.Fatalf("stale/incorrect decoded JPEG phase %d: %d,%d,%d", phase, r>>8, g>>8, b>>8)
			}
		}
	}
}

func TestRestartSourceFixtureAuthenticatesAndDecodesBothStreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := newRestartSourceFixture(t)
	var previous string
	for _, phase := range []int32{0, 1} {
		address, source, stop := f.start(t, phase)
		if address == previous {
			t.Fatal("source endpoint did not change")
		}
		previous = address
		tr, err := f.transport(f.pairing, address)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tr.Close() })
		manifest, err := tr.FetchManifest(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(cameraChannels(manifest, nil)) != 1 || len(microphoneChannels(manifest, nil)) != 1 {
			t.Fatal("source lost a media descriptor")
		}
		frames, closeStream, err := tr.Stream(ctx, []uint32{1, 2})
		if err != nil {
			t.Fatal(err)
		}
		seen := map[uint32]bool{}
		for len(seen) < 2 {
			select {
			case frame, ok := <-frames:
				if !ok {
					t.Fatal("source ended before both media frames")
				}
				switch frame.ChannelID {
				case 1:
					assertRestartJPEG(t, frame.Payload, phase)
				case 2:
					if !bytes.Equal(frame.Payload, bytes.Repeat(restartPCMMarker(phase), 2)) {
						t.Fatal("PCM samples changed in transport")
					}
				default:
					t.Fatal("unexpected channel")
				}
				seen[frame.ChannelID] = true
			case <-ctx.Done():
				t.Fatal("no complete media frames")
			}
		}
		_ = closeStream()
		_ = tr.Close()
		stop()
		if source.subscribed.Load() != 1 {
			t.Fatal("unexpected stream count")
		}
	}
	if f.verified.Load() != 2 {
		t.Fatalf("verified TLS consumers = %d", f.verified.Load())
	}
}
