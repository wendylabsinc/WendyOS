package services

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// fakeLoopback is a recording double for cameraLoopback: it never touches the
// kernel, so tests can assert on the calls VideoService makes without a real
// v4l2loopback module (or a Linux host) present. newIPTestService installs one
// by default so no test in this file exercises the real ipcam.Loopback's
// module-detection path (which shells out to modprobe on Linux); tests that
// care about a specific call type-assert s.loopback back to *fakeLoopback.
type fakeLoopback struct {
	mu sync.Mutex

	availableErr error
	nodePaths    map[uint32]string

	acquireCount map[uint32]int
	releaseCount map[uint32]int

	ensureNodesCalls int
	ensureNodesErr   error

	containerConsumers [][]string
	credChanged        []uint32
	removed            []uint32
	shutdownCalled     bool

	// Auxiliary-node state backs the two-plane camera data path. auxNext is the
	// number the next allocation hands out; auxErr forces the band-exhausted
	// path; auxCreated and auxRemoved record the calls.
	auxNext    int
	auxErr     error
	auxCreated []int
	auxRemoved []int
}

func newFakeLoopback() *fakeLoopback {
	return &fakeLoopback{
		nodePaths:    make(map[uint32]string),
		acquireCount: make(map[uint32]int),
		releaseCount: make(map[uint32]int),
	}
}

func (f *fakeLoopback) AllocateAuxNodeNumber() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.auxErr != nil {
		return 0, f.auxErr
	}
	if f.auxNext == 0 {
		f.auxNext = 255
	}
	nr := f.auxNext
	f.auxNext--
	return nr, nil
}

func (f *fakeLoopback) EnsureAuxNode(_ context.Context, nr int, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auxCreated = append(f.auxCreated, nr)
	return nil
}

func (f *fakeLoopback) RemoveAuxNode(nr int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auxRemoved = append(f.auxRemoved, nr)
}

func (f *fakeLoopback) Available() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.availableErr
}

func (f *fakeLoopback) EnsureNodes(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureNodesCalls++
	return f.ensureNodesErr
}

func (f *fakeLoopback) EnsureNode(_ context.Context, id uint32, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ensureNodesErr == nil {
		f.nodePaths[id] = fmt.Sprintf("/dev/video%d", id)
	}
	return f.ensureNodesErr
}

func (f *fakeLoopback) NodePath(camID uint32) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path, ok := f.nodePaths[camID]
	return path, ok
}

func (f *fakeLoopback) AcquireView(camID uint32) func() {
	f.mu.Lock()
	f.acquireCount[camID]++
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.releaseCount[camID]++
			f.mu.Unlock()
		})
	}
}

func (f *fakeLoopback) SetContainerConsumers(containerIDs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containerConsumers = append(f.containerConsumers, append([]string(nil), containerIDs...))
}

func (f *fakeLoopback) CredentialsChanged(camID uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.credChanged = append(f.credChanged, camID)
}

func (f *fakeLoopback) RemoveCamera(camID uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, camID)
}

func (f *fakeLoopback) Shutdown() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdownCalled = true
}

// fakeVideoStream is a minimal ServerStreamingServer so tests drive the real
// StreamVideo path rather than a nil-stream special case in production code.
// Only Context and Send are ever called; the embedded interface supplies the
// rest of the method set.
type fakeVideoStream struct {
	grpc.ServerStreamingServer[agentpb.VideoFrame]
	ctx context.Context

	mu     sync.Mutex
	frames [][]byte
}

func newFakeVideoStream(ctx context.Context) *fakeVideoStream {
	return &fakeVideoStream{ctx: ctx}
}

func (f *fakeVideoStream) Context() context.Context { return f.ctx }

func (f *fakeVideoStream) Send(frame *agentpb.VideoFrame) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, frame.GetData())
	return nil
}

// newIPTestService returns a service with state under a temporary directory and
// no physical cameras, so tests exercise only the network camera paths.
func newIPTestService(t *testing.T) *VideoService {
	t.Helper()
	dir := t.TempDir()
	s := NewVideoService(context.Background(), zap.NewNop())
	t.Cleanup(s.Shutdown)
	s.registry = ipcam.NewRegistry(filepath.Join(dir, "cameras.json"))
	if err := s.registry.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	s.credentials = ipcam.NewCredentialStore(filepath.Join(dir, "credentials.json"))
	if err := s.credentials.Load(); err != nil {
		t.Fatalf("credential load: %v", err)
	}
	s.globDevices = func() ([]string, error) { return nil, nil }
	s.enumerateLibcamera = func(ctx context.Context) (map[string]string, error) { return nil, nil }
	// Reachable by default so tests exercise the path under test rather than the
	// preflight; the tests that care about reachability override this.
	s.cameraReachable = func(string) bool { return true }
	// Replaces the real ipcam.Loopback that NewVideoService just constructed
	// (pointed, at that point, at the default /var/lib/wendy paths rather than
	// the temp-dir registry/credentials just assigned above). Beyond that
	// staleness, exercising the real module-detection path in every test that
	// touches a registered camera would mean shelling out to modprobe on every
	// Linux run; the fake keeps these tests deterministic and fast on every
	// platform. Tests asserting on a specific loopback call type-assert this
	// back to *fakeLoopback; tests simulating "no loopback wired in" overwrite
	// it with nil instead.
	s.loopback = newFakeLoopback()
	return s
}

func registerTestCamera(t *testing.T, s *VideoService) ipcam.Camera {
	t.Helper()
	cam, err := s.registry.Upsert(ipcam.Camera{
		MAC:     "ec:71:db:2a:ae:7e",
		Address: "10.98.0.50",
		Model:   "RLC-520A",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return cam
}

// A registered camera appears in the listing with the IP transport and its
// address, and reports that it has no credentials yet.
func TestListIncludesIPCameras(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)

	resp, err := s.ListVideoDevices(context.Background(), &agentpb.ListVideoDevicesRequest{})
	if err != nil {
		t.Fatalf("ListVideoDevices: %v", err)
	}
	devices := resp.GetDevices()
	if len(devices) != 1 {
		t.Fatalf("got %d devices, want 1", len(devices))
	}
	d := devices[0]
	if d.GetId() != cam.ID {
		t.Fatalf("id = %d, want %d", d.GetId(), cam.ID)
	}
	if d.GetTransport() != agentpb.VideoTransport_VIDEO_TRANSPORT_IP {
		t.Fatalf("transport = %v, want IP", d.GetTransport())
	}
	if d.GetAddress() != "10.98.0.50" || d.GetMac() != "ec:71:db:2a:ae:7e" {
		t.Fatalf("address/mac = %q/%q", d.GetAddress(), d.GetMac())
	}
	if d.GetName() != "RLC-520A" {
		t.Fatalf("name = %q, want the model", d.GetName())
	}
	if d.GetHasCredentials() {
		t.Fatal("has_credentials true before any login")
	}
	// No loopback node exists yet, so the path is empty rather than a lie.
	if d.GetPath() != "" {
		t.Fatalf("path = %q, want empty", d.GetPath())
	}
}

// A camera with no discovered model still lists as something readable.
func TestListIPCameraWithoutModel(t *testing.T) {
	s := newIPTestService(t)
	if _, err := s.registry.Upsert(ipcam.Camera{MAC: "aa:bb:cc:dd:ee:ff", Address: "10.98.0.7"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	resp, err := s.ListVideoDevices(context.Background(), &agentpb.ListVideoDevicesRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := resp.GetDevices()[0].GetName(); got != "network camera" {
		t.Fatalf("name = %q, want a readable fallback", got)
	}
}

func TestSetCameraCredentialsMarksHasCredentials(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)

	if _, err := s.SetCameraCredentials(context.Background(), &agentpb.SetCameraCredentialsRequest{
		DeviceId: cam.ID, Username: "admin", Password: "hunter2",
	}); err != nil {
		t.Fatalf("SetCameraCredentials: %v", err)
	}
	resp, err := s.ListVideoDevices(context.Background(), &agentpb.ListVideoDevicesRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !resp.GetDevices()[0].GetHasCredentials() {
		t.Fatal("has_credentials false after login")
	}
	// The secret must not be reachable through the listing.
	for _, d := range resp.GetDevices() {
		if strings.Contains(d.String(), "hunter2") {
			t.Fatalf("password leaked into the listing: %s", d.String())
		}
	}
}

func TestSetCameraCredentialsRejectsUnknownID(t *testing.T) {
	s := newIPTestService(t)
	_, err := s.SetCameraCredentials(context.Background(), &agentpb.SetCameraCredentialsRequest{
		DeviceId: 240, Username: "admin", Password: "x",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}
}

func TestSetCameraCredentialsRequiresUsername(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)
	_, err := s.SetCameraCredentials(context.Background(), &agentpb.SetCameraCredentialsRequest{
		DeviceId: cam.ID, Password: "x",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestForgetCameraRemovesItAndItsCredentials(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)
	if _, err := s.SetCameraCredentials(context.Background(), &agentpb.SetCameraCredentialsRequest{
		DeviceId: cam.ID, Username: "admin", Password: "hunter2",
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := s.ForgetCamera(context.Background(), &agentpb.ForgetCameraRequest{DeviceId: cam.ID}); err != nil {
		t.Fatalf("ForgetCamera: %v", err)
	}
	if _, ok := s.registry.Get(cam.ID); ok {
		t.Fatal("camera still registered after forget")
	}
	if s.credentials.Has(cam.MAC) {
		t.Fatal("credentials outlived the camera")
	}
}

func TestForgetCameraUnknownID(t *testing.T) {
	s := newIPTestService(t)
	_, err := s.ForgetCamera(context.Background(), &agentpb.ForgetCameraRequest{DeviceId: 250})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}
}

func TestResolveSourceV4L2AndIP(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)

	got, err := s.resolveSource(0)
	if err != nil {
		t.Fatalf("resolveSource(0): %v", err)
	}
	if got.kind != sourceV4L2 || got.path != "/dev/video0" || got.key != "/dev/video0" {
		t.Fatalf("v4l2 source = %+v", got)
	}

	got, err = s.resolveSource(cam.ID)
	if err != nil {
		t.Fatalf("resolveSource(%d): %v", cam.ID, err)
	}
	if got.kind != sourceIP {
		t.Fatalf("kind = %v, want sourceIP", got.kind)
	}
	if got.key != "ip:200" {
		t.Fatalf("key = %q, want ip:200", got.key)
	}
	if got.path != "" {
		t.Fatalf("path = %q, want empty for a network camera", got.path)
	}
	if got.camera.MAC != cam.MAC {
		t.Fatalf("camera = %+v", got.camera)
	}
}

// An ID inside the reserved band with no registered camera is NotFound, not a
// silent attempt to open /dev/video203.
func TestResolveSourceUnregisteredBandID(t *testing.T) {
	s := newIPTestService(t)
	_, err := s.resolveSource(ipcam.IDBandStart + 3)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}
}

// Streaming a camera with no credentials must fail with the machine-readable
// reason the command-line interface turns into a login hint.
func TestStreamVideoWithoutCredentials(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)

	err := s.StreamVideo(&agentpb.StreamVideoRequest{DeviceId: cam.ID},
		newFakeVideoStream(context.Background()))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
	if !streamreason.Has(err, reasonIPCameraNoCredentials) {
		t.Fatalf("error %v missing reason %s", err, reasonIPCameraNoCredentials)
	}
}

// A registered camera with no address cannot be dialled, and the message must
// name the camera rather than leaking a malformed URL.
func TestStreamVideoIPWithoutAddress(t *testing.T) {
	s := newIPTestService(t)
	cam, err := s.registry.Upsert(ipcam.Camera{MAC: "aa:bb:cc:dd:ee:ff", Model: "RLC-520A"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.credentials.Set(cam.MAC, ipcam.Credential{Username: "admin", Password: "p"}); err != nil {
		t.Fatalf("set credential: %v", err)
	}
	err = s.StreamVideo(&agentpb.StreamVideoRequest{DeviceId: cam.ID},
		newFakeVideoStream(context.Background()))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("error = %v, want the unreachable description", err)
	}
	if strings.Contains(err.Error(), "rtsp://") {
		t.Fatalf("error leaked a stream URL: %v", err)
	}
}

// Network cameras send whatever resolution they are configured for, so the V4L2
// resolution allowlist must not apply: 2560x1920 is a real RLC-520A main-stream
// size and is not in that list.
func TestStreamVideoIPSkipsResolutionAllowlist(t *testing.T) {
	args := captureIPPipelineArgs(t, 2560, 1920)
	if !strings.Contains(args, "h264Preview_01_main") {
		t.Fatalf("wide request did not select the main stream: %s", args)
	}
	// The pipeline must never carry the password in a form a log would show.
	if !strings.Contains(args, "rtsp://admin:p@") {
		t.Fatalf("pipeline url = %s, want credentials in the location", args)
	}
}

// A narrow request takes the sub-stream, which is the cheap smooth feed.
func TestStreamVideoIPNarrowRequestUsesSubStream(t *testing.T) {
	args := captureIPPipelineArgs(t, 640, 480)
	if !strings.Contains(args, "h264Preview_01_sub") {
		t.Fatalf("narrow request did not select the sub stream: %s", args)
	}
}

// captureIPPipelineArgs streams a credentialed camera at the given size and
// returns the pipeline arguments the producer built. The capture subprocess is
// replaced, so nothing is executed.
func captureIPPipelineArgs(t *testing.T, width, height uint32) string {
	t.Helper()
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)
	if err := s.credentials.Set(cam.MAC, ipcam.Credential{Username: "admin", Password: "p"}); err != nil {
		t.Fatalf("set credential: %v", err)
	}

	// The producer runs on its own goroutine, so the arguments come back over a
	// channel rather than through a shared variable.
	got := make(chan []string, 1)
	s.runGStreamer = func(ctx context.Context, args []string, onFrame func([]byte)) error {
		select {
		case got <- args:
		default:
		}
		return nil
	}

	err := s.StreamVideo(&agentpb.StreamVideoRequest{
		DeviceId: cam.ID, Width: width, Height: height,
	}, newFakeVideoStream(context.Background()))
	// The fake capture returns immediately, so the producer stops and the stream
	// ends with that, not with an argument-validation error.
	if status.Code(err) == codes.InvalidArgument {
		t.Fatalf("network stream rejected by parameter validation: %v", err)
	}

	select {
	case args := <-got:
		return strings.Join(args, " ")
	case <-time.After(5 * time.Second):
		t.Fatal("capture pipeline was never started")
		return ""
	}
}

// A device ID above the band is still rejected by the existing bound.
func TestStreamVideoRejectsOutOfRangeID(t *testing.T) {
	s := newIPTestService(t)
	err := s.StreamVideo(&agentpb.StreamVideoRequest{DeviceId: 300},
		newFakeVideoStream(context.Background()))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

// Frames the capture pipeline emits must reach the gRPC stream as H.264.
func TestStreamVideoIPForwardsFrames(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)
	if err := s.credentials.Set(cam.MAC, ipcam.Credential{Username: "admin", Password: "p"}); err != nil {
		t.Fatalf("set credential: %v", err)
	}
	s.runGStreamer = func(ctx context.Context, args []string, onFrame func([]byte)) error {
		onFrame([]byte{0x00, 0x00, 0x00, 0x01, 0x67})
		return nil
	}

	fake := newFakeVideoStream(context.Background())
	// The producer ends after its single frame, so StreamVideo returns the
	// producer-stopped error; the frame must still have been delivered.
	_ = s.StreamVideo(&agentpb.StreamVideoRequest{DeviceId: cam.ID}, fake)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.frames) == 0 {
		t.Fatal("no frames reached the stream")
	}
	if got := fake.frames[0]; len(got) != 5 || got[4] != 0x67 {
		t.Fatalf("frame = %v, want the bytes the pipeline emitted", got)
	}
}

// RefreshCameras must return the listing even when a probe round fails, so a
// device with no multicast route still shows what it already knows.
func TestRefreshCamerasSurvivesProbeFailure(t *testing.T) {
	s := newIPTestService(t)
	registerTestCamera(t, s)
	resp, err := s.RefreshCameras(context.Background(), &agentpb.RefreshCamerasRequest{})
	if err != nil {
		t.Fatalf("RefreshCameras: %v", err)
	}
	if len(resp.GetDevices()) != 1 {
		t.Fatalf("got %d devices, want 1", len(resp.GetDevices()))
	}
}

// A service with no network camera state must behave exactly as before, since
// that is every device that has never seen one.
func TestListWithoutRegistryIsUnaffected(t *testing.T) {
	s := NewVideoService(context.Background(), zap.NewNop())
	t.Cleanup(s.Shutdown)
	s.registry = nil
	s.credentials = nil
	s.globDevices = func() ([]string, error) { return nil, nil }
	s.enumerateLibcamera = func(ctx context.Context) (map[string]string, error) { return nil, nil }

	resp, err := s.ListVideoDevices(context.Background(), &agentpb.ListVideoDevicesRequest{})
	if err != nil {
		t.Fatalf("ListVideoDevices: %v", err)
	}
	if len(resp.GetDevices()) != 0 {
		t.Fatalf("got %d devices, want 0", len(resp.GetDevices()))
	}
	if _, err := s.SetCameraCredentials(context.Background(),
		&agentpb.SetCameraCredentialsRequest{DeviceId: 200, Username: "u"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", status.Code(err))
	}
}

// A camera that cannot be reached must fail immediately and say so. Without the
// preflight the RTSP connect is black-holed, takes twenty seconds to time out,
// and reports a generic pipeline failure that names neither the camera nor the
// address.
func TestStreamVideoRefusesUnreachableCamera(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)
	if _, err := s.SetCameraCredentials(context.Background(), &agentpb.SetCameraCredentialsRequest{
		DeviceId: cam.ID, Username: "admin", Password: "hunter2",
	}); err != nil {
		t.Fatalf("SetCameraCredentials: %v", err)
	}

	var dialled string
	s.cameraReachable = func(address string) bool {
		dialled = address
		return false
	}
	s.runGStreamer = func(context.Context, []string, func([]byte)) error {
		t.Error("capture started against an unreachable camera")
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := s.StreamVideo(&agentpb.StreamVideoRequest{DeviceId: cam.ID}, newFakeVideoStream(ctx))
	if err == nil {
		t.Fatal("StreamVideo succeeded against an unreachable camera")
	}
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", got)
	}
	if !strings.Contains(err.Error(), "10.98.0.50") {
		t.Fatalf("error %q does not name the address", err)
	}
	if dialled != "10.98.0.50" {
		t.Fatalf("dialled %q, want the camera's address", dialled)
	}
}

// The reachability test uses the RTSP port, not the registry's Online flag. That
// flag comes from a probe of port 80, which some cameras do not serve, so gating
// on it would refuse cameras that stream perfectly well.
func TestStreamVideoStreamsOfflineButReachableCamera(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)
	if _, err := s.SetCameraCredentials(context.Background(), &agentpb.SetCameraCredentialsRequest{
		DeviceId: cam.ID, Username: "admin", Password: "hunter2",
	}); err != nil {
		t.Fatalf("SetCameraCredentials: %v", err)
	}
	// Explicitly offline by the port-80 probe.
	s.registry.MarkSeen(cam.MAC, "", false)
	if got, _ := s.registry.Get(cam.ID); got.Online {
		t.Fatal("camera is online, want the offline case under test")
	}

	started := make(chan struct{})
	s.runGStreamer = func(ctx context.Context, _ []string, _ func([]byte)) error {
		close(started)
		<-ctx.Done()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- s.StreamVideo(&agentpb.StreamVideoRequest{DeviceId: cam.ID}, newFakeVideoStream(ctx))
	}()

	select {
	case <-started:
	case err := <-done:
		t.Fatalf("StreamVideo returned %v instead of streaming a reachable camera", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for capture to start")
	}
	cancel()
	<-done
}

// A v4l2loopback node numbered inside the reserved network-camera band would
// otherwise glob-enumerate as an indistinguishable local device, double-listing
// the same camera: once correctly through the registry, once bogusly as a local
// node with no address, no credentials flag, and the wrong transport.
func TestListCameras_SkipsGlobNodesInIPCameraBand(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)
	nodePath := fmt.Sprintf("/dev/video%d", cam.ID)
	s.globDevices = func() ([]string, error) { return []string{nodePath}, nil }
	// The loopback node itself opens and reports VIDEO_CAPTURE like any other
	// V4L2 device; hasVideoCapture alone cannot tell it apart from a local one.
	s.hasVideoCapture = func(path string) bool { return path == nodePath }

	devices, err := s.listCameras(context.Background())
	if err != nil {
		t.Fatalf("listCameras: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("got %d devices, want exactly 1 (the glob-enumerated node in the IP band must not double-list): %+v", len(devices), devices)
	}
	if devices[0].GetId() != cam.ID || devices[0].GetTransport() != agentpb.VideoTransport_VIDEO_TRANSPORT_IP {
		t.Fatalf("device = %+v, want the single IP-transport registry entry", devices[0])
	}
}

// Once EnsureNodes (or the pump supervisor) has created a camera's loopback
// node, the listing must name it: a container-visible /dev/video<id> a caller
// cannot yet learn about is not meaningfully different from one that does not
// exist.
func TestListIPCameras_PathFilledWhenLoopbackNodeExists(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)
	fake := s.loopback.(*fakeLoopback)
	wantPath := fmt.Sprintf("/dev/video%d", cam.ID)
	fake.nodePaths[cam.ID] = wantPath

	resp, err := s.ListVideoDevices(context.Background(), &agentpb.ListVideoDevicesRequest{})
	if err != nil {
		t.Fatalf("ListVideoDevices: %v", err)
	}
	if got := resp.GetDevices()[0].GetPath(); got != wantPath {
		t.Fatalf("path = %q, want %q", got, wantPath)
	}
}

// Without a loopback node the path must stay empty rather than naming a device
// node that does not exist yet.
func TestListIPCameras_PathEmptyWithoutNode(t *testing.T) {
	s := newIPTestService(t)
	registerTestCamera(t, s)
	// s.loopback is the harness's default fakeLoopback, with no node recorded.

	resp, err := s.ListVideoDevices(context.Background(), &agentpb.ListVideoDevicesRequest{})
	if err != nil {
		t.Fatalf("ListVideoDevices: %v", err)
	}
	if got := resp.GetDevices()[0].GetPath(); got != "" {
		t.Fatalf("path = %q, want empty (no loopback node exists)", got)
	}
}

// Attaching to a network camera's stream counts as a `camera view` consumer
// per the spec ("started when ... `camera view` attaches"), and must release
// that ref once streaming ends rather than pinning the pump up forever.
func TestStreamIPCamera_AcquiresAndReleasesViewRef(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)
	if err := s.credentials.Set(cam.MAC, ipcam.Credential{Username: "admin", Password: "p"}); err != nil {
		t.Fatalf("set credential: %v", err)
	}
	fake := s.loopback.(*fakeLoopback)
	s.runGStreamer = func(ctx context.Context, args []string, onFrame func([]byte)) error {
		onFrame([]byte{0x00, 0x00, 0x00, 0x01, 0x67})
		return nil
	}

	// The fake capture returns immediately, so the producer stops and
	// StreamVideo returns with it; by then the view ref must have been both
	// acquired and released.
	_ = s.StreamVideo(&agentpb.StreamVideoRequest{DeviceId: cam.ID}, newFakeVideoStream(context.Background()))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.acquireCount[cam.ID] != 1 {
		t.Fatalf("acquire count = %d, want 1", fake.acquireCount[cam.ID])
	}
	if fake.releaseCount[cam.ID] != 1 {
		t.Fatalf("release count = %d, want 1 (the view ref must be released once streaming ends)", fake.releaseCount[cam.ID])
	}
}

// A build with no v4l2loopback wired in (s.loopback nil) must still stream a
// network camera through the direct hub path: the loopback integration is
// best-effort and must never gate streaming.
func TestStreamIPCamera_StreamsWhenLoopbackUnavailable(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)
	if err := s.credentials.Set(cam.MAC, ipcam.Credential{Username: "admin", Password: "p"}); err != nil {
		t.Fatalf("set credential: %v", err)
	}
	s.loopback = nil
	s.runGStreamer = func(ctx context.Context, args []string, onFrame func([]byte)) error {
		onFrame([]byte{0x00, 0x00, 0x00, 0x01, 0x67})
		return nil
	}

	fake := newFakeVideoStream(context.Background())
	_ = s.StreamVideo(&agentpb.StreamVideoRequest{DeviceId: cam.ID}, fake)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.frames) == 0 {
		t.Fatal("no frames reached the stream with s.loopback == nil")
	}
}

// Storing a login must nudge the loopback supervisor: a pump already demanded
// but blocked on missing credentials can start now, and a running one with a
// stale login needs to pick the new one up.
func TestSetCameraCredentials_NudgesLoopback(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)
	fake := s.loopback.(*fakeLoopback)

	if _, err := s.SetCameraCredentials(context.Background(), &agentpb.SetCameraCredentialsRequest{
		DeviceId: cam.ID, Username: "admin", Password: "hunter2",
	}); err != nil {
		t.Fatalf("SetCameraCredentials: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.credChanged) != 1 || fake.credChanged[0] != cam.ID {
		t.Fatalf("CredentialsChanged calls = %v, want [%d]", fake.credChanged, cam.ID)
	}
}

// Forgetting a camera must remove its loopback node along with its registry
// entry and credentials, or a container-visible /dev/videoN node would outlive
// the camera it named.
func TestForgetCamera_RemovesLoopback(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)
	fake := s.loopback.(*fakeLoopback)

	if _, err := s.ForgetCamera(context.Background(), &agentpb.ForgetCameraRequest{DeviceId: cam.ID}); err != nil {
		t.Fatalf("ForgetCamera: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.removed) != 1 || fake.removed[0] != cam.ID {
		t.Fatalf("RemoveCamera calls = %v, want [%d]", fake.removed, cam.ID)
	}
}

// TestCameraCredentials validates a stored login by probing the camera's RTSP
// port directly, without starting a capture pipeline (unlike StreamVideo's
// preflight, which would have to start a producer just to learn "401"). A
// probe that accepts the credentials must surface as RESULT_OK, carrying the
// camera's address and the probe's redacted detail string verbatim.
func TestTestCameraCredentials_OK(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)
	if err := s.credentials.Set(cam.MAC, ipcam.Credential{Username: "admin", Password: "hunter2"}); err != nil {
		t.Fatalf("set credential: %v", err)
	}
	s.probeCamera = func(_ context.Context, c ipcam.Camera, cred ipcam.Credential) (ipcam.ProbeResult, string) {
		if c.ID != cam.ID {
			t.Fatalf("probeCamera called with camera %+v, want id %d", c, cam.ID)
		}
		if cred.Username != "admin" || cred.Password != "hunter2" {
			t.Fatalf("probeCamera called with credential %+v, want the stored login", cred)
		}
		return ipcam.ProbeOK, "camera 200 at 10.98.0.50 accepted the credentials"
	}

	resp, err := s.TestCameraCredentials(context.Background(), &agentpb.TestCameraCredentialsRequest{DeviceId: cam.ID})
	if err != nil {
		t.Fatalf("TestCameraCredentials: %v", err)
	}
	if resp.GetResult() != agentpb.TestCameraCredentialsResponse_RESULT_OK {
		t.Fatalf("result = %v, want RESULT_OK", resp.GetResult())
	}
	if resp.GetAddress() != cam.Address {
		t.Fatalf("address = %q, want %q", resp.GetAddress(), cam.Address)
	}
	if resp.GetDetail() != "camera 200 at 10.98.0.50 accepted the credentials" {
		t.Fatalf("detail = %q, want the probe's detail string verbatim", resp.GetDetail())
	}
}

// A probe that rejects the login must surface as RESULT_AUTH_FAILED, a value
// the RPC returns as ordinary response data rather than a gRPC error: bad
// credentials are an expected outcome of a credential test, not a failure of
// the RPC itself.
func TestTestCameraCredentials_AuthFailed(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)
	if err := s.credentials.Set(cam.MAC, ipcam.Credential{Username: "admin", Password: "wrong"}); err != nil {
		t.Fatalf("set credential: %v", err)
	}
	s.probeCamera = func(context.Context, ipcam.Camera, ipcam.Credential) (ipcam.ProbeResult, string) {
		return ipcam.ProbeAuthFailed, "camera 200 at 10.98.0.50 rejected the credentials (RTSP 401)"
	}

	resp, err := s.TestCameraCredentials(context.Background(), &agentpb.TestCameraCredentialsRequest{DeviceId: cam.ID})
	if err != nil {
		t.Fatalf("TestCameraCredentials: %v", err)
	}
	if resp.GetResult() != agentpb.TestCameraCredentialsResponse_RESULT_AUTH_FAILED {
		t.Fatalf("result = %v, want RESULT_AUTH_FAILED", resp.GetResult())
	}
	if !strings.Contains(resp.GetDetail(), "rejected") {
		t.Fatalf("detail = %q, want it to describe the rejection", resp.GetDetail())
	}
}

// A probe that cannot reach the camera at all must surface as
// RESULT_UNREACHABLE, again as data rather than an RPC error.
func TestTestCameraCredentials_Unreachable(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)
	if err := s.credentials.Set(cam.MAC, ipcam.Credential{Username: "admin", Password: "hunter2"}); err != nil {
		t.Fatalf("set credential: %v", err)
	}
	s.probeCamera = func(context.Context, ipcam.Camera, ipcam.Credential) (ipcam.ProbeResult, string) {
		return ipcam.ProbeUnreachable, "camera 200 at 10.98.0.50 is unreachable"
	}

	resp, err := s.TestCameraCredentials(context.Background(), &agentpb.TestCameraCredentialsRequest{DeviceId: cam.ID})
	if err != nil {
		t.Fatalf("TestCameraCredentials: %v", err)
	}
	if resp.GetResult() != agentpb.TestCameraCredentialsResponse_RESULT_UNREACHABLE {
		t.Fatalf("result = %v, want RESULT_UNREACHABLE", resp.GetResult())
	}
	if !strings.Contains(resp.GetDetail(), "unreachable") {
		t.Fatalf("detail = %q, want it to describe the unreachable camera", resp.GetDetail())
	}
}

// A camera with no stored login must fail exactly the way StreamVideo does:
// FailedPrecondition with the IP_CAMERA_NO_CREDENTIALS reason, verbatim, so
// the CLI's existing `camera login` diagnostic applies with zero new mapping.
// The probe seam must not be reached at all.
func TestTestCameraCredentials_NoCredentialsReason(t *testing.T) {
	s := newIPTestService(t)
	cam := registerTestCamera(t, s)
	s.probeCamera = func(context.Context, ipcam.Camera, ipcam.Credential) (ipcam.ProbeResult, string) {
		t.Fatal("probeCamera must not be called without stored credentials")
		return ipcam.ProbeUnreachable, ""
	}

	_, err := s.TestCameraCredentials(context.Background(), &agentpb.TestCameraCredentialsRequest{DeviceId: cam.ID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
	if !streamreason.Has(err, reasonIPCameraNoCredentials) {
		t.Fatalf("error %v missing reason %s", err, reasonIPCameraNoCredentials)
	}
}

// A local (V4L2) device ID has no credentials to test at all, and must be
// rejected before any probe is attempted.
func TestTestCameraCredentials_LocalCameraRejected(t *testing.T) {
	s := newIPTestService(t)
	_, err := s.TestCameraCredentials(context.Background(), &agentpb.TestCameraCredentialsRequest{DeviceId: 0})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
	if !strings.Contains(err.Error(), "local camera") {
		t.Fatalf("error %v does not explain that this is a local camera", err)
	}
}

// An unregistered ID inside the reserved network-camera band is NotFound, the
// same as every other RPC that resolves a device ID through resolveSource.
func TestTestCameraCredentials_UnknownCameraNotFound(t *testing.T) {
	s := newIPTestService(t)
	_, err := s.TestCameraCredentials(context.Background(),
		&agentpb.TestCameraCredentialsRequest{DeviceId: ipcam.IDBandStart + 5})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}
}
