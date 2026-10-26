package services

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/data/live"
	"github.com/wendylabsinc/wendy/go/internal/agent/localsocket"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

var liveMarkersStream = appconfig.RecordingStream{Mode: "lightweight", MediaType: "application/protobuf", Schema: "wendy.data.v1.VideoMarkers"}

// fakeDataRecordStream is a StreamData server stream for direct handler calls.
type fakeDataRecordStream struct {
	grpc.ServerStream
	ctx     context.Context
	records chan *agentpbv2.DataRecord
	headers chan struct{}
}

func newFakeDataRecordStream(ctx context.Context) *fakeDataRecordStream {
	return &fakeDataRecordStream{ctx: ctx, records: make(chan *agentpbv2.DataRecord, 16), headers: make(chan struct{}, 1)}
}
func (f *fakeDataRecordStream) Context() context.Context { return f.ctx }
func (f *fakeDataRecordStream) SendHeader(metadata.MD) error {
	f.headers <- struct{}{}
	return nil
}
func (f *fakeDataRecordStream) Send(r *agentpbv2.DataRecord) error {
	f.records <- r
	return nil
}

func TestAuthorizeDataStream(t *testing.T) {
	tenant := "spiffe://wendy.sh/tenant/" + meshTestTenant
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want codes.Code
	}{
		{"local admin socket", localAdminSocketCtx(), codes.OK},
		{"legacy user certificate", ctxWithURN("urn:wendy:org:2:user:alice"), codes.OK},
		{"tenant user certificate", ctxWithPeerCert(certWithURN(t, userPrincipal(meshTestTenant, "alice"))), codes.OK},
		{"tenant service account", ctxWithPeerCert(certWithURN(t, tenant+"/service/video-relay")), codes.OK},
		{"plaintext provisioning listener", plaintextTCPCtx(), codes.PermissionDenied},
		{"no peer", context.Background(), codes.PermissionDenied},
		{"legacy device certificate", ctxWithURN("urn:wendy:org:2:asset:345"), codes.PermissionDenied},
		{"tenant device certificate", ctxWithPeerCert(certWithURN(t, meshTestDevice)), codes.PermissionDenied},
		{"service relaying a device", ctxWithPeerCert(certWithURN(t, tenant+"/service/asset-345")), codes.PermissionDenied},
		{"code signer", ctxWithPeerCert(certWithURN(t, tenant+"/signer/release")), codes.PermissionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := status.Code(authorizeDataStream(tc.ctx)); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLiveSelection(t *testing.T) {
	sel, err := liveSelection(&agentpbv2.StreamDataRequest{StreamIds: []string{"apps/a/s"}, StreamPrefixes: []string{"_streams/"}, Schemas: []string{"wendy.data.v1.VideoMarkers"}, SendLatest: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.StreamIDs) != 1 || len(sel.StreamPrefixes) != 1 || len(sel.Schemas) != 1 || !sel.SendLatest || sel.MaxRateHz != defaultStreamDataRateHz {
		t.Fatalf("selection: %+v", sel)
	}
	for _, rate := range []float32{0.01, 2.5, 1000} {
		if sel, err = liveSelection(&agentpbv2.StreamDataRequest{MaxRateHz: rate}); err != nil || sel.MaxRateHz != float64(rate) {
			t.Fatalf("rate %v: %+v %v", rate, sel, err)
		}
	}
	for _, rate := range []float32{-1, 0.001, 1001, float32(math.NaN()), float32(math.Inf(1))} {
		if _, err = liveSelection(&agentpbv2.StreamDataRequest{MaxRateHz: rate}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("rate %v: got %v, want InvalidArgument", rate, err)
		}
	}
	// Any schema an app stream may declare can be selected.
	if _, err = liveSelection(&agentpbv2.StreamDataRequest{Schemas: []string{strings.Repeat("x", appconfig.MaxRecordingSchemaBytes)}}); err != nil {
		t.Fatalf("longest app schema: %v", err)
	}
}

func TestStreamDataRejectsBadSelection(t *testing.T) {
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(m)
	tooMany := make([]string, maxStreamDataIDs+1)
	for i := range tooMany {
		tooMany[i] = "apps/a/s"
	}
	long, longSchema := []string{strings.Repeat("x", maxStreamDataIDBytes+1)}, []string{strings.Repeat("x", appconfig.MaxRecordingSchemaBytes+1)}
	var requests []*agentpbv2.StreamDataRequest
	for _, ids := range [][]string{tooMany, {""}} {
		requests = append(requests, &agentpbv2.StreamDataRequest{StreamIds: ids}, &agentpbv2.StreamDataRequest{StreamPrefixes: ids}, &agentpbv2.StreamDataRequest{Schemas: ids})
	}
	requests = append(requests, &agentpbv2.StreamDataRequest{StreamIds: long}, &agentpbv2.StreamDataRequest{StreamPrefixes: long}, &agentpbv2.StreamDataRequest{Schemas: longSchema})
	for _, req := range requests {
		err := service.StreamData(req, newFakeDataRecordStream(localAdminSocketCtx()))
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("selection %v: got %v, want InvalidArgument", req, err)
		}
	}
	if m.Live().Subscribed() {
		t.Fatal("a refused request left a subscriber behind")
	}
}

func TestStreamDataSubscriberCap(t *testing.T) {
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < live.DefaultLimits.MaxSubscribers; i++ {
		s, err := m.Live().Subscribe(live.Selection{})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
	}
	err = NewDataService(m).StreamData(&agentpbv2.StreamDataRequest{}, newFakeDataRecordStream(localAdminSocketCtx()))
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("got %v, want ResourceExhausted", err)
	}
}

// The handler releases its subscription when the client goes away, so a
// dropped viewer does not hold one of the hub's slots.
func TestStreamDataReleasesSubscriptionOnCancel(t *testing.T) {
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(localAdminSocketCtx())
	stream := newFakeDataRecordStream(ctx)
	done := make(chan error, 1)
	go func() { done <- NewDataService(m).StreamData(&agentpbv2.StreamDataRequest{}, stream) }()
	<-stream.headers
	if !m.Live().Subscribed() {
		t.Fatal("headers sent before the subscription was in place")
	}
	cancel()
	if err := <-done; status.Code(err) != codes.Canceled {
		t.Fatalf("got %v, want Canceled", err)
	}
	if m.Live().Subscribed() {
		t.Fatal("subscription outlived its stream")
	}
}

// gatedDataRecordStream holds each Send until the test releases it, so the
// test knows which record the handler is sending while it offers more.
type gatedDataRecordStream struct {
	*fakeDataRecordStream
	release chan struct{}
}

func (g *gatedDataRecordStream) Send(r *agentpbv2.DataRecord) error {
	g.records <- r
	select {
	case <-g.release:
		return nil
	case <-g.ctx.Done():
		return g.ctx.Err()
	}
}

// A record that names the sample it describes carries it to the viewer.
func TestStreamDataSendsSample(t *testing.T) {
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(localAdminSocketCtx())
	defer cancel()
	stream := newFakeDataRecordStream(ctx)
	go func() { _ = NewDataService(m).StreamData(&agentpbv2.StreamDataRequest{}, stream) }()
	<-stream.headers
	descriptor := &recordingpb.StreamDescriptor{MediaType: "application/protobuf", Schema: "wendy.data.v1.VideoMarkers"}
	if err := m.Publish("test/markers", descriptor, &recordingpb.Record{Payload: []byte("boxes"), Inputs: []*recordingpb.SampleRef{{SourceId: "v4l2:/dev/video0", SampleId: 42}}}); err != nil {
		t.Fatal(err)
	}
	if r := <-stream.records; r.GetStreamId() != "agent/test/markers" || r.GetSourceId() != "v4l2:/dev/video0" || r.GetSampleId() != 42 {
		t.Fatalf("record: %v", r)
	}
}

// A viewer is told how many records of a stream it missed while it was busy
// sending, and receives the newest.
func TestStreamDataSendsSkipped(t *testing.T) {
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(localAdminSocketCtx())
	defer cancel()
	stream := &gatedDataRecordStream{fakeDataRecordStream: newFakeDataRecordStream(ctx), release: make(chan struct{})}
	go func() { _ = NewDataService(m).StreamData(&agentpbv2.StreamDataRequest{MaxRateHz: 1000}, stream) }()
	<-stream.headers
	record := func(payload string) {
		t.Helper()
		if _, err := m.RecordStream("test.app", "", "markers", liveMarkersStream, &recordingpb.Record{Payload: []byte(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	record("1")
	if r := <-stream.records; string(r.GetPayload()) != "1" || r.GetSkipped() != 0 {
		t.Fatalf("first: %v", r)
	}
	for _, p := range []string{"2", "3", "4"} {
		record(p)
	}
	stream.release <- struct{}{}
	if r := <-stream.records; string(r.GetPayload()) != "4" || r.GetSkipped() != 2 {
		t.Fatalf("after a busy send: %v", r)
	}
}

// openLiveStream starts StreamData and waits for its headers, after which
// every accepted record is offered to it.
func openLiveStream(t *testing.T, client agentpbv2.DataServiceClient, ids ...string) agentpbv2.DataService_StreamDataClient {
	t.Helper()
	return openLiveStreamWith(t, client, &agentpbv2.StreamDataRequest{StreamIds: ids})
}

func openLiveStreamWith(t *testing.T, client agentpbv2.DataServiceClient, req *agentpbv2.StreamDataRequest) agentpbv2.DataService_StreamDataClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	stream, err := client.StreamData(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if md, err := stream.Header(); err != nil || md == nil {
		_, recvErr := stream.Recv()
		t.Fatalf("no subscription: %v %v", err, recvErr)
	}
	return stream
}

func recvLive(t *testing.T, stream agentpbv2.DataService_StreamDataClient) *agentpbv2.DataRecord {
	t.Helper()
	r, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func serveDataService(t *testing.T, lis net.Listener, m *data.Manager, opts ...grpc.ServerOption) {
	t.Helper()
	server := grpc.NewServer(opts...)
	agentpbv2.RegisterDataServiceServer(server, NewDataService(m))
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
}

func dialDataService(t *testing.T, target string, opts ...grpc.DialOption) agentpbv2.DataServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return agentpbv2.NewDataServiceClient(conn)
}

// localDataService serves m on a unix socket, as the agent's local admin
// socket does, and dials it.
func localDataService(t *testing.T, m *data.Manager) agentpbv2.DataServiceClient {
	t.Helper()
	// Not t.TempDir: unix socket paths are limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "wds")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "agent.sock")
	lis, err := localsocket.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	serveDataService(t, lis, m)
	return dialDataService(t, "unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// An app record reaches a viewer on the agent's local socket, the way an
// on-device page reads it today, with the stream id, schema and payload the
// app's stream declared, and a selection keeps other streams out.
func TestStreamDataOverLocalSocket(t *testing.T) {
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := localDataService(t, m)
	all := openLiveStream(t, client)
	selected := openLiveStream(t, client, "apps/test.app/worker/markers")

	record := func(stream string, payload string) {
		t.Helper()
		if _, err := m.RecordStream("test.app", "worker", stream, liveMarkersStream, &recordingpb.Record{Payload: []byte(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	before := time.Now().UnixNano()
	record("markers", "first")
	r := recvLive(t, selected)
	if r.GetStreamId() != "apps/test.app/worker/markers" || r.GetSchema() != "wendy.data.v1.VideoMarkers" || string(r.GetPayload()) != "first" || r.GetTimestampUnixNanos() < before {
		t.Fatalf("record: %v", r)
	}
	// Both viewers take "first" before "second" is recorded, or the newest
	// record of the stream would rightly replace it for the slower one.
	if r = recvLive(t, all); string(r.GetPayload()) != "first" {
		t.Fatalf("empty selection: got %q, want %q", r.GetPayload(), "first")
	}
	record("other", "unselected")
	record("markers", "second")
	if r = recvLive(t, selected); string(r.GetPayload()) != "second" {
		t.Fatalf("selection let another stream through: %v", r)
	}
	for _, want := range []string{"unselected", "second"} {
		if r = recvLive(t, all); string(r.GetPayload()) != want {
			t.Fatalf("empty selection: got %q, want %q", r.GetPayload(), want)
		}
	}
}

// send_latest starts a viewer with the catalog and the newest record of the
// streams it selects by prefix and schema, written before it connected.
func TestStreamDataSendLatestAndCatalog(t *testing.T) {
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := localDataService(t, m)
	text := appconfig.RecordingStream{Mode: "lightweight", MediaType: "text/plain"}
	m.AdvertiseAppStream("test.app", "worker", "markers", liveMarkersStream)
	m.AdvertiseAppStream("test.app", "worker", "log", text)
	for _, w := range []struct {
		stream  string
		cfg     appconfig.RecordingStream
		payload string
	}{{"markers", liveMarkersStream, "old boxes"}, {"markers", liveMarkersStream, "boxes"}, {"log", text, "hello"}} {
		if _, err := m.RecordStream("test.app", "worker", w.stream, w.cfg, &recordingpb.Record{Payload: []byte(w.payload)}); err != nil {
			t.Fatal(err)
		}
	}

	viewer := openLiveStreamWith(t, client, &agentpbv2.StreamDataRequest{
		StreamPrefixes: []string{"_streams/", "apps/test.app/"},
		Schemas:        []string{"wendy.agent.services.v2.DataStreamInfo", "wendy.data.v1.VideoMarkers"},
		SendLatest:     true,
	})
	for _, want := range []string{"_streams/apps/test.app/worker/log", "_streams/apps/test.app/worker/markers"} {
		r := recvLive(t, viewer)
		var info agentpbv2.DataStreamInfo
		if err := proto.Unmarshal(r.GetPayload(), &info); err != nil {
			t.Fatal(err)
		}
		if r.GetStreamId() != want || info.GetState() != agentpbv2.DataStreamInfo_STATE_ACTIVE || info.GetPublisher() != "test.app" {
			t.Fatalf("catalog: %s %v, want %s", r.GetStreamId(), &info, want)
		}
	}
	if r := recvLive(t, viewer); r.GetStreamId() != "apps/test.app/worker/markers" || string(r.GetPayload()) != "boxes" {
		t.Fatalf("newest record: %v", r)
	}
	// The log stream's schema is not selected; its next record is not sent,
	// and the next markers record is.
	if _, err := m.RecordStream("test.app", "worker", "log", text, &recordingpb.Record{Payload: []byte("again")}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RecordStream("test.app", "worker", "markers", liveMarkersStream, &recordingpb.Record{Payload: []byte("more boxes")}); err != nil {
		t.Fatal(err)
	}
	if r := recvLive(t, viewer); string(r.GetPayload()) != "more boxes" {
		t.Fatalf("live record after the latest: %v", r)
	}
}

// The pre-provisioning listener is plaintext TCP that anyone on the LAN can
// reach, and DataService is registered on it. Live data must not be served
// there.
func TestStreamDataRefusesPlaintextTCP(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	serveDataService(t, lis, m)
	client := dialDataService(t, lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	stream, err := client.StreamData(context.Background(), &agentpbv2.StreamDataRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("got %v, want PermissionDenied", err)
	}
	if m.Live().Subscribed() {
		t.Fatal("refused caller was subscribed")
	}
}

// tlsCertificateWithURN is a self-signed leaf carrying a Wendy identity, for
// both ends of a real TLS handshake.
func tlsCertificateWithURN(t *testing.T, urn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(urn)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		URIs:         []*url.URL{u},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// Over a real mutual TLS handshake a user is served and a device is refused.
// Chain verification is the mTLS server's job and is not repeated here.
func TestStreamDataOverMutualTLS(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := tlsCertificateWithURN(t, "urn:wendy:org:2:asset:1")
	serveDataService(t, lis, m, grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{server}, ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS12})))
	dial := func(urn string) agentpbv2.DataServiceClient {
		client := tlsCertificateWithURN(t, urn)
		creds := credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{client}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
		return dialDataService(t, "passthrough:///agent", grpc.WithTransportCredentials(creds), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	}

	device, err := dial("urn:wendy:org:2:asset:345").StreamData(context.Background(), &agentpbv2.StreamDataRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = device.Recv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("device certificate: got %v, want PermissionDenied", err)
	}

	user := openLiveStream(t, dial("urn:wendy:org:2:user:alice"))
	if _, err := m.RecordStream("test.app", "", "markers", liveMarkersStream, &recordingpb.Record{Payload: []byte("boxes")}); err != nil {
		t.Fatal(err)
	}
	if r := recvLive(t, user); r.GetStreamId() != "apps/test.app/markers" || string(r.GetPayload()) != "boxes" {
		t.Fatalf("record: %v", r)
	}
}

// A viewer that stops reading fills its flow-control window, and Send then
// blocks. The handler must end the stream after streamDataStallTimeout and
// free its hub slot rather than hold the slot for as long as the connection
// lives, which 32 such viewers would turn into a lockout of every other.
func TestStreamDataEndsStalledStream(t *testing.T) {
	old := streamDataStallTimeout
	streamDataStallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { streamDataStallTimeout = old })
	dir, err := os.MkdirTemp("", "wds")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "agent.sock")
	lis, err := localsocket.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	serveDataService(t, lis, m)
	// A fixed window turns off the client's window growth, so a client that
	// never calls Recv stalls the stream after its first 64 KiB.
	client := dialDataService(t, "unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithInitialWindowSize(64<<10), grpc.WithInitialConnWindowSize(64<<10))
	// No deadline: the stream must end because the agent ends it.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stalled, err := client.StreamData(ctx, &agentpbv2.StreamDataRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stalled.Header(); err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, data.MaxRecordingPacket)
	for _, stream := range []string{"frames0", "frames1", "frames2", "frames3"} {
		if _, err := m.RecordStream("test.app", "", stream, liveMarkersStream, &recordingpb.Record{Payload: frame}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.Live().Subscribed() {
		if time.Now().After(deadline) {
			t.Fatal("a stalled viewer still holds its hub slot")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The viewer reads what was sent before the stall, then the reason.
	for {
		if _, err = stalled.Recv(); err != nil {
			break
		}
	}
	if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "agent ended the stream") {
		t.Fatalf("stalled viewer ended with %v, want the agent's DeadlineExceeded", err)
	}
}
