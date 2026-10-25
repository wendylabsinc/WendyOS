package services

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/data/live"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	systempb "github.com/wendylabsinc/wendy/go/proto/gen/systempb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestSystemAPIGrantFor(t *testing.T) {
	notifications := appconfig.Entitlement{Type: appconfig.EntitlementNotifications}
	readAll := appconfig.Entitlement{Type: appconfig.EntitlementDataRead}
	readDetector := appconfig.Entitlement{Type: appconfig.EntitlementDataRead, Allowlist: []string{"apps/com.example.detector"}}
	for _, tc := range []struct {
		name         string
		entitlements []appconfig.Entitlement
		want         SystemAPIGrant
	}{
		{"none", []appconfig.Entitlement{{Type: appconfig.EntitlementCamera}}, SystemAPIGrant{}},
		{"notifications", []appconfig.Entitlement{notifications}, SystemAPIGrant{Capabilities: []string{SystemAPICapabilityNotifications}}},
		{"data-read for every stream", []appconfig.Entitlement{readAll}, SystemAPIGrant{Capabilities: []string{SystemAPICapabilityDataRead}}},
		{"data-read with an allowlist", []appconfig.Entitlement{notifications, readDetector}, SystemAPIGrant{Capabilities: []string{SystemAPICapabilityNotifications, SystemAPICapabilityDataRead}, DataReadAllowlist: []string{"apps/com.example.detector"}}},
		// App-level and service-level entitlements can both appear. One
		// without an allowlist reads everything, so the narrower one adds
		// nothing.
		{"app and service data-read", []appconfig.Entitlement{readDetector, readAll}, SystemAPIGrant{Capabilities: []string{SystemAPICapabilityDataRead}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SystemAPIGrantFor(tc.entitlements); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSystemAPIRefusesDataReadWithoutLiveData(t *testing.T) {
	useSystemAPIRoot(t)
	manager := NewAppSystemAPISocketManager(t.Context(), zap.NewNop(), &recordingNotificationSender{}, nil)
	if _, err := manager.Ensure("com.example.viewer", "", SystemAPIGrant{Capabilities: []string{SystemAPICapabilityDataRead}}); err == nil {
		t.Fatal("data-read granted with no live data to serve")
	}
}

// useSystemAPIRoot points the System API sockets at a short temporary
// directory: unix socket paths are limited to about 100 bytes.
func useSystemAPIRoot(t *testing.T) {
	t.Helper()
	old := AppSystemAPISocketRootPath
	root, err := os.MkdirTemp("/tmp", "wsd-")
	if err != nil {
		t.Fatal(err)
	}
	AppSystemAPISocketRootPath = root
	t.Cleanup(func() {
		AppSystemAPISocketRootPath = old
		_ = os.RemoveAll(root)
	})
}

// systemDataTest is an agent with live data and a System API socket per app.
type systemDataTest struct {
	t       *testing.T
	capture *data.Manager
	sockets *AppSystemAPISocketManager
}

func newSystemDataTest(t *testing.T) *systemDataTest {
	t.Helper()
	useSystemAPIRoot(t)
	capture, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sockets := NewAppSystemAPISocketManager(ctx, zap.NewNop(), &recordingNotificationSender{}, capture.Live())
	t.Cleanup(func() { cancel(); sockets.stopAll() })
	return &systemDataTest{t: t, capture: capture, sockets: sockets}
}

// app grants appID's service the entitlements and returns a connection to the
// app's System API socket, as the app's container would dial it.
func (s *systemDataTest) app(appID, service string, entitlements ...appconfig.Entitlement) *grpc.ClientConn {
	s.t.Helper()
	dir, err := s.sockets.Ensure(appID, service, SystemAPIGrantFor(entitlements))
	if err != nil {
		s.t.Fatal(err)
	}
	conn, err := grpc.NewClient("unix://"+filepath.Join(dir, SystemAPISocketFilename), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func (s *systemDataTest) record(appID, stream, payload string) {
	s.t.Helper()
	if _, err := s.capture.RecordStream(appID, "", stream, liveMarkersStream, &recordingpb.Record{Payload: []byte(payload)}); err != nil {
		s.t.Fatal(err)
	}
}

// streamDataError is the status a StreamData call on conn ends with when it
// is refused before any record.
func streamDataError(t *testing.T, conn *grpc.ClientConn, ids ...string) codes.Code {
	t.Helper()
	return streamDataRequestError(t, conn, &agentpbv2.StreamDataRequest{StreamIds: ids})
}

func streamDataRequestError(t *testing.T, conn *grpc.ClientConn, req *agentpbv2.StreamDataRequest) codes.Code {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := agentpbv2.NewDataServiceClient(conn).StreamData(ctx, req)
	if err != nil {
		return status.Code(err)
	}
	_, err = stream.Recv()
	return status.Code(err)
}

// endOf reads stream until it ends and returns how it ended. Records still
// queued for it, such as a send_latest snapshot, come first.
func endOf(t *testing.T, stream agentpbv2.DataService_StreamDataClient) codes.Code {
	t.Helper()
	for {
		if _, err := stream.Recv(); err != nil {
			return status.Code(err)
		}
	}
}

// catalogInfo decodes a catalog record.
func catalogInfo(t *testing.T, r *agentpbv2.DataRecord) *agentpbv2.DataStreamInfo {
	t.Helper()
	info := new(agentpbv2.DataStreamInfo)
	if r.GetSchema() != "wendy.agent.services.v2.DataStreamInfo" || proto.Unmarshal(r.GetPayload(), info) != nil {
		t.Fatalf("not a catalog record: %v", r)
	}
	return info
}

// An app reads live data on its own System API socket with data-read, and
// only what its allowlist admits. An app without data-read reads nothing, and
// data-read reaches no other DataService method.
func TestSystemAPIDataRead(t *testing.T) {
	agent := newSystemDataTest(t)
	viewer := agent.app("com.example.viewer", "", appconfig.Entitlement{Type: appconfig.EntitlementDataRead, Allowlist: []string{"apps/com.example.detector"}})
	notifier := agent.app("com.example.notifier", "", appconfig.Entitlement{Type: appconfig.EntitlementNotifications})

	scoped := openLiveStream(t, agentpbv2.NewDataServiceClient(viewer))
	agent.record("com.example.other", "markers", "not allowed")
	agent.record("com.example.detector2", "markers", "a different app")
	agent.record("com.example.detector", "markers", "boxes")
	if r := recvLive(t, scoped); r.GetStreamId() != "apps/com.example.detector/markers" || string(r.GetPayload()) != "boxes" {
		t.Fatalf("allowlisted record: %v", r)
	}

	if got := streamDataError(t, viewer, "apps/com.example.other/markers"); got != codes.PermissionDenied {
		t.Fatalf("stream outside the allowlist: got %v, want PermissionDenied", got)
	}
	if got := streamDataError(t, notifier); got != codes.PermissionDenied {
		t.Fatalf("app without data-read: got %v, want PermissionDenied", got)
	}

	// data-read is StreamData alone: the rest of DataService, unary or
	// streaming, stays refused, and so does a capability the app lacks.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := agentpbv2.NewDataServiceClient(viewer)
	if _, err := client.Status(ctx, &agentpbv2.DataStatusRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Status: got %v, want PermissionDenied", err)
	}
	export, err := client.ExportRecording(ctx, &agentpbv2.DataRecordingExportRequest{})
	if err == nil {
		_, err = export.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ExportRecording: got %v, want PermissionDenied", err)
	}
	if _, err := systempb.NewNotificationServiceClient(viewer).Send(ctx, validSystemNotificationRequest()); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("notifications without the entitlement: got %v, want PermissionDenied", err)
	}
}

// The services of one app share its socket and cannot be told apart, so the
// app reads the union of their allowlists. A stream keeps the scope it was
// opened with when the scope widens, and ends when it narrows.
func TestSystemAPIDataReadUnionsServices(t *testing.T) {
	agent := newSystemDataTest(t)
	readDetector := appconfig.Entitlement{Type: appconfig.EntitlementDataRead, Allowlist: []string{"apps/com.example.detector"}}
	api := agent.app("com.example.viewer", "api", readDetector)
	client := agentpbv2.NewDataServiceClient(api)
	detectorOnly := openLiveStream(t, client)

	agent.app("com.example.viewer", "worker", appconfig.Entitlement{Type: appconfig.EntitlementDataRead, Allowlist: []string{"apps/com.example.thermal"}})
	both := openLiveStream(t, client)
	// Opened under the wider scope with selectors, a rate and send_latest, it
	// ends with the scope all the same.
	selected := openLiveStreamWith(t, client, &agentpbv2.StreamDataRequest{StreamPrefixes: []string{"apps/com.example.thermal/", "_streams/"}, Schemas: []string{"wendy.data.v1.VideoMarkers", "wendy.agent.services.v2.DataStreamInfo"}, MaxRateHz: 5, SendLatest: true})
	agent.record("com.example.thermal", "grid", "hot")
	if r := recvLive(t, both); r.GetStreamId() != "apps/com.example.thermal/grid" {
		t.Fatalf("union: %v", r)
	}
	agent.record("com.example.detector", "markers", "boxes")
	if r := recvLive(t, detectorOnly); r.GetStreamId() != "apps/com.example.detector/markers" {
		t.Fatalf("stream opened before the scope widened: %v", r)
	}
	if r := recvLive(t, both); r.GetStreamId() != "apps/com.example.detector/markers" {
		t.Fatalf("union: %v", r)
	}

	// The worker leaves: what it granted goes with it, for open streams too.
	agent.sockets.Release("com.example.viewer", "worker")
	if _, err := both.Recv(); status.Code(err) != codes.Aborted {
		t.Fatalf("stream opened under the wider scope: got %v, want Aborted", err)
	}
	if got := endOf(t, selected); got != codes.Aborted {
		t.Fatalf("stream with selectors opened under the wider scope: got %v, want Aborted", got)
	}
	if got := streamDataError(t, api, "apps/com.example.thermal/grid"); got != codes.PermissionDenied {
		t.Fatalf("after the worker left: got %v, want PermissionDenied", got)
	}
	agent.record("com.example.detector", "markers", "more boxes")
	if r := recvLive(t, detectorOnly); string(r.GetPayload()) != "more boxes" {
		t.Fatalf("stream within the remaining scope: %v", r)
	}

	// Withdrawing data-read ends the rest.
	if _, err := agent.sockets.Ensure("com.example.viewer", "api", SystemAPIGrantFor([]appconfig.Entitlement{{Type: appconfig.EntitlementNotifications}})); err != nil {
		t.Fatal(err)
	}
	if _, err := detectorOnly.Recv(); status.Code(err) != codes.Aborted {
		t.Fatalf("stream after data-read was withdrawn: got %v, want Aborted", err)
	}
	if got := streamDataError(t, api); got != codes.PermissionDenied {
		t.Fatalf("after data-read was withdrawn: got %v, want PermissionDenied", got)
	}
}

// A redeploy replaces a service's grant in place, with no Release first. With
// the same grant nothing ends and a stream opened meanwhile reads the whole
// app's scope; a narrower grant ends only the streams it no longer covers.
func TestSystemAPIDataReadSurvivesSiblingRedeploy(t *testing.T) {
	agent := newSystemDataTest(t)
	readDetector := appconfig.Entitlement{Type: appconfig.EntitlementDataRead, Allowlist: []string{"apps/com.example.detector"}}
	readThermal := appconfig.Entitlement{Type: appconfig.EntitlementDataRead, Allowlist: []string{"apps/com.example.thermal"}}
	client := agentpbv2.NewDataServiceClient(agent.app("com.example.viewer", "api", readDetector))
	detectorOnly := openLiveStream(t, client)
	agent.app("com.example.viewer", "worker", readThermal)
	union := openLiveStream(t, client)

	if _, err := agent.sockets.Ensure("com.example.viewer", "worker", SystemAPIGrantFor([]appconfig.Entitlement{readThermal})); err != nil {
		t.Fatal(err)
	}
	reopened := openLiveStream(t, client)
	agent.record("com.example.thermal", "grid", "hot")
	for name, stream := range map[string]agentpbv2.DataService_StreamDataClient{"open": union, "reopened": reopened} {
		if r := recvLive(t, stream); r.GetStreamId() != "apps/com.example.thermal/grid" {
			t.Fatalf("%s stream after an identical redeploy: %v", name, r)
		}
	}

	// The worker comes back reading one thermal stream: the streams opened to
	// read all of thermal end, the one opened before the worker joined does not.
	if _, err := agent.sockets.Ensure("com.example.viewer", "worker", SystemAPIGrantFor([]appconfig.Entitlement{{Type: appconfig.EntitlementDataRead, Allowlist: []string{"apps/com.example.thermal/grid"}}})); err != nil {
		t.Fatal(err)
	}
	if got := endOf(t, union); got != codes.Aborted {
		t.Fatalf("stream over all of thermal after a narrower redeploy: got %v, want Aborted", got)
	}
	if got := endOf(t, reopened); got != codes.Aborted {
		t.Fatalf("reopened stream after a narrower redeploy: got %v, want Aborted", got)
	}
	agent.record("com.example.detector", "markers", "boxes")
	if r := recvLive(t, detectorOnly); string(r.GetPayload()) != "boxes" {
		t.Fatalf("stream opened before the worker joined: %v", r)
	}
}

// narrowingStream is a StreamData server stream that records what it sends
// and runs onSend inside each Send, as a slow reader's Send would block while
// the app's grant changes.
type narrowingStream struct {
	grpc.ServerStream // unused; the methods below are all StreamData calls
	ctx               context.Context
	subbed            chan struct{}
	sent              []string
	onSend            func()
}

func (s *narrowingStream) Context() context.Context { return s.ctx }
func (s *narrowingStream) SendHeader(metadata.MD) error {
	close(s.subbed)
	return nil
}
func (s *narrowingStream) Send(r *agentpbv2.DataRecord) error {
	s.sent = append(s.sent, r.GetStreamId())
	if s.onSend != nil {
		s.onSend()
		s.onSend = nil
	}
	return nil
}

// A narrowed stream ends at once, even for a reader that always has a record
// waiting: it never sends a record the new scope leaves out.
func TestSystemAPIDataReadNarrowStopsWaitingRecords(t *testing.T) {
	hub := live.NewHub(live.Limits{})
	scope := []string{"apps/com.example.detector", "apps/com.example.thermal"}
	svc := &systemDataService{hub: hub, scope: func() ([]string, bool) { return scope, true }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &narrowingStream{ctx: ctx, subbed: make(chan struct{})}
	// While the first record is being sent, a thermal record arrives and the
	// thermal grant goes away.
	out.onSend = func() {
		hub.Offer(&live.Record{StreamID: "apps/com.example.thermal/grid", Payload: []byte("hot")})
		scope = []string{"apps/com.example.detector"}
		svc.narrow()
	}
	done := make(chan error, 1)
	go func() { done <- svc.StreamData(&agentpbv2.StreamDataRequest{}, out) }()
	<-out.subbed
	hub.Offer(&live.Record{StreamID: "apps/com.example.detector/markers", Payload: []byte("boxes")})
	select {
	case err := <-done:
		if status.Code(err) != codes.Aborted {
			t.Fatalf("narrowed stream ended with %v, want Aborted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("narrowed stream did not end")
	}
	if !reflect.DeepEqual(out.sent, []string{"apps/com.example.detector/markers"}) {
		t.Fatalf("sent %q, want only the record from before the narrowing", out.sent)
	}
}

// An app's allowlist bounds the catalog as it bounds the records: the app
// sees the catalog entries of the streams it may read and learns nothing of
// the others, not even that they exist.
func TestSystemAPIDataReadCatalog(t *testing.T) {
	agent := newSystemDataTest(t)
	viewer := agent.app("com.example.viewer", "", appconfig.Entitlement{Type: appconfig.EntitlementDataRead, Allowlist: []string{"apps/com.example.detector"}})
	agent.capture.AdvertiseAppStream("com.example.other", "", "markers", liveMarkersStream)
	agent.capture.AdvertiseAppStream("com.example.detector", "", "markers", liveMarkersStream)
	agent.capture.AdvertiseAgentStream("campaign/door/markers/v4l2:/dev/video0", &recordingpb.StreamDescriptor{MediaType: "application/protobuf", Schema: "wendy.data.v1.VideoMarkers"})

	// What wendy data streams asks for: the whole catalog, at once.
	catalog := openLiveStreamWith(t, agentpbv2.NewDataServiceClient(viewer), &agentpbv2.StreamDataRequest{StreamPrefixes: []string{"_streams/"}, SendLatest: true})
	if r := recvLive(t, catalog); r.GetStreamId() != "_streams/apps/com.example.detector/markers" || catalogInfo(t, r).GetState() != agentpbv2.DataStreamInfo_STATE_IDLE {
		t.Fatalf("current catalog: %v", r)
	}
	// Other streams appearing, going active and going away stay hidden; the
	// next entry is the allowed stream going active.
	agent.capture.AdvertiseAppStream("com.example.other", "", "grid", liveMarkersStream)
	agent.record("com.example.other", "markers", "not allowed")
	agent.capture.UnadvertiseAppStream("com.example.other", "", "markers")
	agent.record("com.example.detector", "markers", "boxes")
	if r := recvLive(t, catalog); r.GetStreamId() != "_streams/apps/com.example.detector/markers" || catalogInfo(t, r).GetState() != agentpbv2.DataStreamInfo_STATE_ACTIVE {
		t.Fatalf("catalog change: %v", r)
	}

	// Naming another stream's catalog entry is refused as naming the stream is.
	if got := streamDataError(t, viewer, "_streams/apps/com.example.other/markers"); got != codes.PermissionDenied {
		t.Fatalf("catalog entry outside the allowlist: got %v, want PermissionDenied", got)
	}
	named := openLiveStreamWith(t, agentpbv2.NewDataServiceClient(viewer), &agentpbv2.StreamDataRequest{StreamIds: []string{"_streams/apps/com.example.detector/markers"}, SendLatest: true})
	if r := recvLive(t, named); r.GetStreamId() != "_streams/apps/com.example.detector/markers" {
		t.Fatalf("allowed catalog entry: %v", r)
	}
}

// The System API socket serves StreamData with the operator's request rules:
// the same validation, selectors and send_latest, within the allowlist.
func TestSystemAPIDataReadSelection(t *testing.T) {
	agent := newSystemDataTest(t)
	viewer := agent.app("com.example.viewer", "", appconfig.Entitlement{Type: appconfig.EntitlementDataRead, Allowlist: []string{"apps/com.example.detector"}})
	for _, req := range []*agentpbv2.StreamDataRequest{{MaxRateHz: 5000}, {StreamPrefixes: []string{""}}, {Schemas: []string{""}}} {
		if got := streamDataRequestError(t, viewer, req); got != codes.InvalidArgument {
			t.Fatalf("%v: got %v, want InvalidArgument", req, got)
		}
	}

	text := appconfig.RecordingStream{Mode: "lightweight", MediaType: "text/plain"}
	agent.capture.AdvertiseAppStream("com.example.detector", "", "markers", liveMarkersStream)
	agent.capture.AdvertiseAppStream("com.example.detector", "", "log", text)
	agent.capture.AdvertiseAppStream("com.example.other", "", "markers", liveMarkersStream)
	agent.record("com.example.other", "markers", "not allowed")
	agent.record("com.example.detector", "markers", "boxes")
	if _, err := agent.capture.RecordStream("com.example.detector", "", "log", text, &recordingpb.Record{Payload: []byte("hello")}); err != nil {
		t.Fatal(err)
	}
	// A prefix wider than the allowlist is narrowed to it, and the schema
	// keeps the markers.
	markers := openLiveStreamWith(t, agentpbv2.NewDataServiceClient(viewer), &agentpbv2.StreamDataRequest{StreamPrefixes: []string{"apps/"}, Schemas: []string{"wendy.data.v1.VideoMarkers"}, SendLatest: true})
	if r := recvLive(t, markers); r.GetStreamId() != "apps/com.example.detector/markers" || string(r.GetPayload()) != "boxes" {
		t.Fatalf("newest allowed record: %v", r)
	}
	agent.record("com.example.other", "markers", "still not allowed")
	agent.record("com.example.detector", "markers", "more boxes")
	if r := recvLive(t, markers); string(r.GetPayload()) != "more boxes" {
		t.Fatalf("live record: %v", r)
	}
}

// The System API socket receives every selection the operator path accepts,
// up to the largest.
func TestSystemAPIDataReadLargestSelection(t *testing.T) {
	agent := newSystemDataTest(t)
	conn := agent.app("com.example.viewer", "", appconfig.Entitlement{Type: appconfig.EntitlementDataRead})
	longest := func(n int) []string {
		values := make([]string, maxStreamDataIDs)
		for i := range values {
			values[i] = fmt.Sprintf("agent/%0*d", n-len("agent/"), i)
		}
		return values
	}
	req := &agentpbv2.StreamDataRequest{StreamIds: longest(maxStreamDataIDBytes), StreamPrefixes: longest(maxStreamDataIDBytes), Schemas: longest(maxStreamDataIDBytes), MaxRateHz: maxStreamDataRateHz, SendLatest: true}
	if _, err := liveSelection(req); err != nil {
		t.Fatalf("liveSelection refused the largest selection: %v", err)
	}
	if got, want := proto.Size(req), maxStreamDataRequestBytes(); got != want {
		t.Fatalf("largest selection is %d bytes, maxStreamDataRequestBytes says %d", got, want)
	}
	openLiveStreamWith(t, agentpbv2.NewDataServiceClient(conn), req)
}

func TestCoversScope(t *testing.T) {
	for _, tc := range []struct {
		scope, within []string
		want          bool
	}{
		{nil, nil, true},
		{nil, []string{"apps/a"}, true},
		{[]string{"apps/a"}, nil, false},
		{[]string{"apps/a"}, []string{"apps/a"}, true},
		{[]string{"apps/a"}, []string{"apps/a/markers"}, true},
		{[]string{"apps/a/markers"}, []string{"apps/a"}, false},
		{[]string{"apps/a", "apps/b"}, []string{"apps/b", "apps/a/x"}, true},
		{[]string{"apps/a"}, []string{"apps/a", "apps/ab"}, false},
	} {
		if got := covers(tc.scope, tc.within); got != tc.want {
			t.Errorf("covers(%q, %q) = %v, want %v", tc.scope, tc.within, got, tc.want)
		}
	}
}

// One app cannot take every live data slot on the device.
func TestSystemAPIDataReadPerAppCap(t *testing.T) {
	agent := newSystemDataTest(t)
	conn := agent.app("com.example.viewer", "", appconfig.Entitlement{Type: appconfig.EntitlementDataRead})
	client := agentpbv2.NewDataServiceClient(conn)
	// Every call counts, whatever it selects.
	selections := []*agentpbv2.StreamDataRequest{
		{},
		{StreamIds: []string{"apps/a/markers"}},
		{StreamPrefixes: []string{"apps/"}},
		{StreamPrefixes: []string{"_streams/"}, SendLatest: true},
		{Schemas: []string{"wendy.data.v1.VideoMarkers"}},
		{MaxRateHz: 1000},
		{StreamPrefixes: []string{"agent/"}, MaxRateHz: 0.5},
		{SendLatest: true},
	}
	if len(selections) != maxAppDataStreams {
		t.Fatalf("%d selections for a cap of %d", len(selections), maxAppDataStreams)
	}
	for _, req := range selections {
		openLiveStreamWith(t, client, req)
	}
	if got := streamDataRequestError(t, conn, &agentpbv2.StreamDataRequest{StreamPrefixes: []string{"_streams/"}}); got != codes.ResourceExhausted {
		t.Fatalf("stream %d: got %v, want ResourceExhausted", maxAppDataStreams+1, got)
	}
	other := agent.app("com.example.other", "", appconfig.Entitlement{Type: appconfig.EntitlementDataRead})
	openLiveStream(t, agentpbv2.NewDataServiceClient(other))
}

// Apps together cannot take every live data slot either: however many
// data-read apps a device runs, operators keep their reserved slots.
func TestSystemAPIDataReadLeavesOperatorSlots(t *testing.T) {
	agent := newSystemDataTest(t)
	readAll := appconfig.Entitlement{Type: appconfig.EntitlementDataRead}
	// openStream reports how a stream started: OK once its subscription is in
	// place. cancel ends it.
	openStream := func(client agentpbv2.DataServiceClient) (code codes.Code, cancel context.CancelFunc) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		stream, err := client.StreamData(ctx, &agentpbv2.StreamDataRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if md, err := stream.Header(); err == nil && md != nil {
			return codes.OK, cancel
		}
		_, err = stream.Recv()
		return status.Code(err), cancel
	}
	appSlots := live.DefaultLimits.MaxSubscribers - reservedOperatorDataStreams
	var open []context.CancelFunc
	for i := 0; len(open) < appSlots; i++ {
		client := agentpbv2.NewDataServiceClient(agent.app(fmt.Sprintf("com.example.app%d", i), "", readAll))
		for j := 0; j < maxAppDataStreams && len(open) < appSlots; j++ {
			code, cancel := openStream(client)
			if code != codes.OK {
				t.Fatalf("app stream %d: %v", len(open)+1, code)
			}
			open = append(open, cancel)
		}
	}
	late := agentpbv2.NewDataServiceClient(agent.app("com.example.late", "", readAll))
	if code, _ := openStream(late); code != codes.ResourceExhausted {
		t.Fatalf("app stream past the apps' share: got %v, want ResourceExhausted", code)
	}
	for i := 0; i < reservedOperatorDataStreams; i++ {
		sub, err := agent.capture.Live().Subscribe(live.Selection{})
		if err != nil {
			t.Fatalf("operator subscription %d: %v", i+1, err)
		}
		t.Cleanup(sub.Close)
	}

	// A stream that ends gives its slot back.
	open[0]()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		code, _ := openStream(late)
		if code == codes.OK {
			break
		}
		if code != codes.ResourceExhausted || time.Now().After(deadline) {
			t.Fatalf("after an app stream ended: got %v, want OK", code)
		}
	}
}
