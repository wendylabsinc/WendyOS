package backend

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

type fakeDesktopLite struct {
	liteConnection
	mu           sync.Mutex
	done         chan struct{}
	once         sync.Once
	listener     func(*sensorlinkpb.SensorData)
	unsubscribed chan uint32
	infoErr      error
	conf         *litepb.WendyConf
	mode         liteclient.ConfPushMode
	reset        bool
	console      chan liteclient.ConsoleChunk
	detached     chan struct{}
}

func newFakeDesktopLite() *fakeDesktopLite {
	return &fakeDesktopLite{done: make(chan struct{}), unsubscribed: make(chan uint32, 8), console: make(chan liteclient.ConsoleChunk, 4), detached: make(chan struct{}, 8)}
}
func (f *fakeDesktopLite) Close() error          { f.once.Do(func() { close(f.done) }); return nil }
func (f *fakeDesktopLite) Done() <-chan struct{} { return f.done }
func (f *fakeDesktopLite) GetDeviceInfo(time.Duration) (*liteclient.DeviceInfo, error) {
	return &liteclient.DeviceInfo{OS: "wendy-lite", OSVersion: "1.2.3", Target: "esp32s3", CPUArchitecture: "xtensa"}, f.infoErr
}
func (f *fakeDesktopLite) GetSensorManifest(time.Duration) (*sensorlinkpb.SensorManifest, error) {
	return &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{{ChannelId: 0, Name: "ESP camera", Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG}}}}}, nil
}
func (f *fakeDesktopLite) AddSensorDataListener(fn func(*sensorlinkpb.SensorData)) func() {
	f.mu.Lock()
	f.listener = fn
	f.mu.Unlock()
	return func() { f.mu.Lock(); f.listener = nil; f.mu.Unlock() }
}
func (f *fakeDesktopLite) SensorLinkSubscribe(ids []uint32, _ time.Duration) error {
	f.mu.Lock()
	listener := f.listener
	f.mu.Unlock()
	// Emit during subscribe to exercise listener-before-ack ordering and assembly.
	listener(&sensorlinkpb.SensorData{ChannelId: ids[0], FrameSeq: 7, Payload: []byte{0xff, 0xd8}})
	listener(&sensorlinkpb.SensorData{ChannelId: ids[0], FrameSeq: 7, ChunkSeq: 1, Flags: 3, Payload: []byte{0xff, 0xd9}})
	return nil
}
func (f *fakeDesktopLite) SensorLinkUnsubscribe(ids []uint32, _ time.Duration) error {
	f.unsubscribed <- ids[0]
	return nil
}
func (f *fakeDesktopLite) ConsoleAttach(bool, bool) (<-chan liteclient.ConsoleChunk, func(bool) error, error) {
	return f.console, func(bool) error { f.detached <- struct{}{}; return nil }, nil
}
func (f *fakeDesktopLite) StopApp() error { return nil }
func (f *fakeDesktopLite) PushConf(conf *litepb.WendyConf, mode liteclient.ConfPushMode, _ func(uint32, uint32)) error {
	f.conf, f.mode = conf, mode
	return nil
}
func (f *fakeDesktopLite) ResetTargetDevice(bool, time.Duration) error { f.reset = true; return nil }
func connectFakeLite(t *testing.T, b *Backend, f *fakeDesktopLite) uint64 {
	t.Helper()
	b.connectTarget = func(context.Context, string) (*grpcclient.AgentConnection, liteConnection, error) { return nil, f, nil }
	session, err := b.Connect("cloud://lite", false)
	if err != nil {
		t.Fatal(err)
	}
	return session
}
func nextLiteEvent(t *testing.T, b *Backend) Event {
	t.Helper()
	select {
	case e := <-b.events:
		return e
	case <-time.After(time.Second):
		t.Fatal("no event")
		return Event{}
	}
}
func TestLiteDesktopSessionCameraConsoleAndWiFi(t *testing.T) {
	b := New()
	defer b.Shutdown()
	f := newFakeDesktopLite()
	session := connectFakeLite(t, b, f)
	if !b.IsLite(session) {
		t.Fatal("Lite session not identified")
	}
	info, err := b.DeviceInfo(session)
	if err != nil || info.GetBoardModel() != "esp32s3" || info.GetOsVersion() != "1.2.3" {
		t.Fatalf("info = %v, %v", info, err)
	}
	if _, err := b.Apps(); err == nil {
		t.Fatal("Lite exposed Linux apps")
	}
	cameras, err := b.Cameras()
	if err != nil || len(cameras) != 1 || cameras[0].Id != 0 {
		t.Fatalf("cameras = %v, %v", cameras, err)
	}
	if err := b.StartCamera(0, 12, session+1); err == nil {
		t.Fatal("accepted stale session")
	}
	if err := b.StartCamera(0, 12, session); err != nil {
		t.Fatal(err)
	}
	event := nextLiteEvent(t, b)
	if event.Kind != EventJPEG || event.Session != session || event.Stream != 12 || string(event.Bytes) != "\xff\xd8\xff\xd9" {
		t.Fatalf("JPEG event = %#v", event)
	}
	if err := b.StopCamera(0, 12, session); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.unsubscribed:
	default:
		t.Fatal("camera not unsubscribed")
	}
	if err := b.StartLogs("", 13); err != nil {
		t.Fatal(err)
	}
	f.console <- liteclient.ConsoleChunk{Data: []byte("console"), Stderr: true}
	event = nextLiteEvent(t, b)
	if event.Log.Body != "console" || event.Log.Level != LogError || event.Stream != 13 {
		t.Fatalf("console = %#v", event)
	}
	if err := b.JoinWiFi(session+1, "home", "secret", 0, false); err == nil {
		t.Fatal("stale Wi-Fi write")
	}
	if err := b.JoinWiFi(session, "home", "secret", 0, false); err != nil {
		t.Fatal(err)
	}
	if !f.reset || f.mode != liteclient.ConfPushModeUpdate || f.conf.GetWifi().GetNetworks()[0].GetSsid() != "home" {
		t.Fatal("Wi-Fi did not update and reboot")
	}
	if err := b.Disconnect(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.done:
	default:
		t.Fatal("Lite connection leaked")
	}
	select {
	case <-f.detached:
	default:
		t.Fatal("console not detached")
	}
}
func TestFailedLiteProbeKeepsCurrentConnection(t *testing.T) {
	b := New()
	defer b.Shutdown()
	current := newFakeDesktopLite()
	session := connectFakeLite(t, b, current)
	failed := newFakeDesktopLite()
	failed.infoErr = errors.New("offline")
	b.connectTarget = func(context.Context, string) (*grpcclient.AgentConnection, liteConnection, error) {
		return nil, failed, nil
	}
	if _, err := b.Connect("another", false); err == nil {
		t.Fatal("accepted failed probe")
	}
	if b.session != session || b.lite != current {
		t.Fatal("failed attempt replaced current session")
	}
	select {
	case <-failed.done:
	default:
		t.Fatal("failed connection leaked")
	}
}
func TestCancelledLiteConnectClosesLateResult(t *testing.T) {
	b := New()
	defer b.Shutdown()
	started, release := make(chan struct{}), make(chan struct{})
	f := newFakeDesktopLite()
	b.connectTarget = func(context.Context, string) (*grpcclient.AgentConnection, liteConnection, error) {
		close(started)
		<-release
		return nil, f, nil
	}
	request := b.ReserveConnection()
	result := make(chan error, 1)
	go func() { _, err := b.ConnectRequest("lite", false, request); result <- err }()
	<-started
	b.ReserveConnection()
	close(release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	select {
	case <-f.done:
	default:
		t.Fatal("cancelled connection leaked")
	}
	if b.lite != nil {
		t.Fatal("cancelled connection installed")
	}
}

func TestLiteRelayContextLivesUntilDisconnect(t *testing.T) {
	b := New()
	defer b.Shutdown()
	f := newFakeDesktopLite()
	var relayCtx context.Context
	b.connectTarget = func(ctx context.Context, _ string) (*grpcclient.AgentConnection, liteConnection, error) {
		relayCtx = ctx
		return nil, f, nil
	}
	if _, err := b.Connect("cloud://lite", false); err != nil {
		t.Fatal(err)
	}
	if err := relayCtx.Err(); err != nil {
		t.Fatalf("setup completion cancelled relay: %v", err)
	}
	if err := b.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if relayCtx.Err() == nil {
		t.Fatal("disconnect did not cancel relay")
	}
}

func TestLiteReconnectReleasesExclusiveTransportBeforeDial(t *testing.T) {
	b := New()
	defer b.Shutdown()
	old := newFakeDesktopLite()
	session := connectFakeLite(t, b, old)
	next := newFakeDesktopLite()
	b.connectTarget = func(context.Context, string) (*grpcclient.AgentConnection, liteConnection, error) {
		select {
		case <-old.done:
		default:
			t.Error("serial port still held during reconnect")
		}
		return nil, next, nil
	}
	newSession, err := b.Connect("cloud://lite", false)
	if err != nil || newSession <= session || b.lite != next {
		t.Fatalf("reconnect = %d, %v", newSession, err)
	}
}
