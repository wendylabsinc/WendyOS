// Package backend is the process-local Go implementation linked by SwiftUI.
package backend

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/thermal"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type EventKind uint32

const (
	EventLoginURL EventKind = iota + 1
	EventLoginDone
	EventLoginError
	EventLog
	EventLogsError
	EventVideo
	EventCameraError
	EventJPEG
	EventThermal
	EventVPNError
)

type Event struct {
	Kind            EventKind
	Session, Stream uint64
	Text            string
	Bytes           []byte
	Log             *LogRecord
	Thermal         *ThermalFrame
}
type Backend struct {
	nearby                  nearbySetup
	UnitreeControl          UnitreeController
	modelRun                *ModelRun
	chatTurn                *ChatTurn
	chatEnvironment         *desktopChatEnvironment
	chatToolsFactory        desktopChatToolsFactory
	deviceSelector          string
	appForwards             map[uint32]*appForward
	connectMu               sync.Mutex
	connectSequence         uint64
	connectCancel           context.CancelFunc
	top                     commands.DesktopTopSampler
	pinSelector, pinTarget  string
	connectTarget           func(context.Context, string) (*grpcclient.AgentConnection, liteConnection, error)
	lite                    liteConnection
	connectDevice           func(context.Context, string) (*grpcclient.AgentConnection, error)
	unpinDevice             func(string) error
	cameraFirstFrameTimeout time.Duration
	// Operations are serialized. Stream producers communicate only through events.
	mu                  sync.Mutex
	conn                *grpcclient.AgentConnection
	life                context.Context
	stop                context.CancelFunc
	logsStop, loginStop context.CancelFunc
	logsDone            <-chan struct{}
	cameraDevices       map[uint32]*pb.VideoDevice
	videoStops          map[uint32]cameraSubscription
	session             uint64
	events              chan Event
	relay               *vpn.Relay
	vpnProbes           *vpnProbes
}

func New() *Backend {
	return &Backend{events: make(chan Event, 64), cameraFirstFrameTimeout: 15 * time.Second,
		connectTarget: connectDesktopTarget, unpinDevice: commands.DesktopUnpin}
}

// IdentityRecoveryError exposes a recovery action without exposing a caller-
// supplied unpin target. The backend retains the target from the CLI refusal.
type IdentityRecoveryError struct{ Err error }

func (e *IdentityRecoveryError) Error() string { return e.Err.Error() }
func (e *IdentityRecoveryError) Unwrap() error { return e.Err }
func (b *Backend) emit(ctx context.Context, event Event) {
	select {
	case b.events <- event:
	case <-ctx.Done():
	}
}
func (b *Backend) emitText(ctx context.Context, kind EventKind, session, stream uint64, text string) {
	b.emit(ctx, Event{Kind: kind, Session: session, Stream: stream, Text: text})
}
func (b *Backend) Poll() []Event {
	out := make([]Event, 0)
	for len(out) < 32 {
		select {
		case e := <-b.events:
			out = append(out, e)
		default:
			return out
		}
	}
	return out
}

type cameraSubscription struct {
	stream uint64
	stop   context.CancelFunc
	done   <-chan struct{}
}

func (b *Backend) stopCameras() {
	for id, subscription := range b.videoStops {
		subscription.stop()
		if subscription.done != nil {
			<-subscription.done
		}
		delete(b.videoStops, id)
	}
}
func (b *Backend) disconnect() {
	b.cameraDevices = nil
	if b.chatEnvironment != nil {
		b.chatEnvironment.close()
		b.chatEnvironment = nil
	}
	b.deviceSelector = ""
	if b.modelRun != nil {
		b.modelRun.cancel()
		b.modelRun = nil
	}
	if b.chatTurn != nil {
		b.chatTurn.cancel()
		b.chatTurn = nil
	}
	for _, forward := range b.appForwards {
		forward.Close()
	}
	b.appForwards = nil
	b.top = commands.DesktopTopSampler{}
	if b.logsStop != nil {
		b.logsStop()
		b.logsStop = nil
	}
	if b.logsDone != nil {
		<-b.logsDone
		b.logsDone = nil
	}
	b.stopCameras()
	if b.stop != nil {
		b.stop()
		b.stop = nil
	}
	if b.lite != nil {
		b.lite.Close()
		b.lite = nil
	}
	if b.conn != nil {
		b.conn.Close()
		b.conn = nil
	}
	b.session++
}

// Public operations serialize access to the CLI connection and configuration.
func (b *Backend) begin() (context.Context, func()) {
	b.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	return ctx, func() { cancel(); b.mu.Unlock() }
}
func (b *Backend) connection(session uint64) (*grpcclient.AgentConnection, error) {
	if err := b.validSession(session); err != nil {
		return nil, err
	}
	if b.lite != nil {
		return nil, errors.New("this operation requires a WendyOS agent and is unavailable on Wendy Lite")
	}
	return b.conn, nil
}
func (b *Backend) Initialize(cli string) error {
	_, done := b.begin()
	defer done()
	return commands.InitializeDesktop(cli)
}
func (b *Backend) Nearby() ([]commands.DesktopDevice, error) {
	ctx, done := b.begin()
	defer done()
	return commands.DesktopNearby(ctx)
}
func (b *Backend) Cloud() ([]commands.DesktopDevice, error) {
	ctx, done := b.begin()
	defer done()
	return commands.DesktopCloud(ctx)
}
func (b *Backend) Login(development bool) error {
	_, done := b.begin()
	defer done()
	if b.loginStop != nil {
		b.loginStop()
	}
	loginCtx, stop := context.WithTimeout(context.Background(), 5*time.Minute)
	b.loginStop = stop
	go func() {
		defer stop()
		err := commands.DesktopLogin(loginCtx, development, func(url string) { b.emitText(loginCtx, EventLoginURL, 0, 0, url) })
		if errors.Is(err, context.Canceled) {
			return
		}
		delivery, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err != nil {
			b.emitText(delivery, EventLoginError, 0, 0, err.Error())
		} else {
			b.emit(delivery, Event{Kind: EventLoginDone})
		}
	}()
	return nil
}
func (b *Backend) CancelLogin() error {
	_, done := b.begin()
	defer done()
	if b.loginStop != nil {
		b.loginStop()
	}
	return nil
}

// ReserveConnection cancels an in-flight dial without locking ordinary device
// operations. Swift calls this before enqueueing a connection attempt.
func (b *Backend) ReserveConnection() uint64 {
	b.connectMu.Lock()
	defer b.connectMu.Unlock()
	b.connectSequence++
	if b.connectCancel != nil {
		b.connectCancel()
		b.connectCancel = nil
	}
	return b.connectSequence
}
func (b *Backend) Connect(selector string, forgetIdentity bool) (uint64, error) {
	return b.ConnectRequest(selector, forgetIdentity, b.ReserveConnection())
}
func (b *Backend) ConnectRequest(selector string, forgetIdentity bool, request uint64) (uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	b.connectMu.Lock()
	if request != b.connectSequence {
		b.connectMu.Unlock()
		return 0, context.Canceled
	}
	b.connectCancel = cancel
	b.connectMu.Unlock()

	b.mu.Lock()
	if ctx.Err() != nil {
		b.mu.Unlock()
		return 0, ctx.Err()
	}
	if forgetIdentity {
		if b.pinTarget == "" || b.pinSelector != selector {
			b.mu.Unlock()
			return 0, errors.New("connect to this device before forgetting its saved identity")
		}
		if err := b.unpinDevice(b.pinTarget); err != nil {
			b.mu.Unlock()
			return 0, &IdentityRecoveryError{Err: err}
		}
	}
	b.pinSelector, b.pinTarget = "", ""
	// USB Lite connections own an exclusive serial port. Release it before redialing the same board.
	if b.lite != nil && b.deviceSelector == selector {
		b.disconnect()
	}
	b.mu.Unlock()

	// A Lite cloud relay retains its dial context for the stream lifetime.
	// Forward setup cancellation until commit, then keep it until disconnect.
	targetCtx, targetCancel := context.WithCancel(context.Background())
	cancelSetup := context.AfterFunc(ctx, targetCancel)
	committed := false
	defer func() {
		cancelSetup()
		if !committed {
			targetCancel()
		}
	}()
	// Keep the current connection and its streams alive while dialing.
	var conn *grpcclient.AgentConnection
	var lite liteConnection
	var err error
	if b.connectDevice != nil {
		conn, err = b.connectDevice(targetCtx, selector)
	} else {
		conn, lite, err = b.connectTarget(targetCtx, selector)
	}
	if err == nil && lite != nil {
		client := lite
		interrupt := context.AfterFunc(ctx, func() { client.Close() })
		_, err = lite.GetDeviceInfo(3 * time.Second)
		interrupt()
		if err != nil {
			lite.Close()
			lite = nil
		}
	}
	if err == nil && conn == nil && lite == nil {
		err = errors.New("device connector returned no connection")
	}
	if err == nil && conn != nil {
		_, err = conn.AgentService.GetAgentVersion(ctx, &pb.GetAgentVersionRequest{})
		if err != nil {
			conn.Close()
			conn = nil
		}
	}

	cancelSetup()
	b.mu.Lock()
	b.connectMu.Lock()
	if request != b.connectSequence || ctx.Err() != nil {
		sameRequest := request == b.connectSequence
		b.connectMu.Unlock()
		b.mu.Unlock()
		if lite != nil {
			lite.Close()
		}
		if conn != nil {
			conn.Close()
		}
		if sameRequest && ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, context.Canceled
	}
	b.connectCancel = nil
	if err != nil {
		if target := commands.DesktopPinRecovery(err); target != "" {
			b.pinSelector, b.pinTarget = selector, target
			err = &IdentityRecoveryError{Err: err}
		}
		b.connectMu.Unlock()
		b.mu.Unlock()
		return 0, err
	}
	previousConnection, previousLite := b.conn, b.lite
	b.lite = nil
	b.conn = nil
	b.disconnect()
	b.conn, b.lite = conn, lite
	b.deviceSelector = selector
	b.life, b.stop = targetCtx, targetCancel
	committed = true
	session := b.session
	b.connectMu.Unlock()
	b.mu.Unlock()
	// A tunnel's Close may wait for transport shutdown; never hold UI control
	// or device-operation locks while doing that work.
	if previousLite != nil {
		previousLite.Close()
	}
	if previousConnection != nil {
		previousConnection.Close()
	}
	return session, nil
}

func (b *Backend) Disconnect() error {
	b.ReserveConnection()
	_, done := b.begin()
	defer done()
	b.pinSelector, b.pinTarget = "", ""
	b.disconnect()
	return nil
}
func (b *Backend) StartVPN(routes []vpn.Route) (vpn.Config, error) {
	_, done := b.begin()
	defer done()
	b.stopVPN()
	dial := reportVPNDialFailures(desktopDial, func(message string) {
		// A full UI event queue must not stall a failed network flow.
		select {
		case b.events <- Event{Kind: EventVPNError, Text: message}:
		default:
		}
	})
	probes := newVPNProbes(routes, commands.DesktopConnect)
	relay, config, err := vpn.NewRelay(routes, dial, probes.Probe)
	if err != nil {
		probes.Close()
		return vpn.Config{}, err
	}
	b.relay, b.vpnProbes = relay, probes
	return config, nil
}
func (b *Backend) StopVPN() error {
	_, done := b.begin()
	defer done()
	b.stopVPN()
	return nil
}

// Caller holds b.mu, including when replacing routes or shutting down the app.
func (b *Backend) stopVPN() {
	if b.vpnProbes != nil {
		b.vpnProbes.Close()
		b.vpnProbes = nil
	}
	if b.relay != nil {
		b.relay.Close()
		b.relay = nil
	}
}
func (b *Backend) Shutdown() error {
	b.NearbyClose(0)
	b.UnitreeControl.Shutdown()
	b.ReserveConnection()
	_, done := b.begin()
	defer done()
	b.disconnect()
	if b.loginStop != nil {
		b.loginStop()
	}
	b.stopVPN()
	return nil
}
func (b *Backend) Apps() ([]*pb.AppContainer, error) {
	ctx, done := b.begin()
	defer done()
	conn, err := b.connection(0)
	if err != nil {
		return nil, err
	}
	return listDesktopApps(ctx, conn)
}

func listDesktopApps(ctx context.Context, conn *grpcclient.AgentConnection) ([]*pb.AppContainer, error) {
	stream, err := conn.ContainerService.ListContainers(ctx, &pb.ListContainersRequest{})
	if err != nil {
		return nil, err
	}
	apps := []*pb.AppContainer{}
	for {
		v, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return apps, nil
		}
		if err != nil {
			return nil, err
		}
		if v.Container != nil {
			apps = append(apps, v.Container)
		}
	}
}
func (b *Backend) Cameras() ([]*pb.VideoDevice, error) {
	ctx, done := b.begin()
	defer done()
	if b.lite != nil {
		return b.liteCameras()
	}
	conn, err := b.connection(0)
	if err != nil {
		return nil, err
	}
	v, err := conn.VideoService.ListVideoDevices(ctx, &pb.ListVideoDevicesRequest{})
	if err != nil {
		return nil, err
	}
	b.cameraDevices = make(map[uint32]*pb.VideoDevice, len(v.Devices))
	for _, d := range v.Devices {
		b.cameraDevices[d.GetId()] = d
	}
	return v.Devices, nil
}
func (b *Backend) StopLogs() error {
	_, done := b.begin()
	defer done()
	err := b.validSession(0)
	if err != nil {
		return err
	}
	if b.logsStop != nil {
		b.logsStop()
	}
	return nil
}
func (b *Backend) StopCamera(camera uint32, streamID, sessionID uint64) error {
	_, done := b.begin()
	defer done()
	err := b.validSession(sessionID)
	if err != nil {
		return err
	}
	if subscription, ok := b.videoStops[camera]; ok && (streamID == 0 || subscription.stream == streamID) {
		subscription.stop()
		if subscription.done != nil {
			<-subscription.done
		}
		delete(b.videoStops, camera)
	}
	return nil
}
func (b *Backend) StopCameras(sessionID uint64) error {
	_, done := b.begin()
	defer done()
	err := b.validSession(sessionID)
	if err != nil {
		return err
	}
	b.stopCameras()
	return nil
}
func (b *Backend) StartLogs(app string, streamID uint64) error {
	_, done := b.begin()
	defer done()
	if b.lite != nil {
		return b.startLiteLogs(app, streamID)
	}
	conn, err := b.connection(0)
	if err != nil {
		return err
	}
	if b.logsStop != nil {
		b.logsStop()
	}
	streamCtx, stop := context.WithCancel(b.life)
	b.logsStop = stop
	last := int32(200)
	req := &pb.StreamLogsRequest{LastN: &last}
	if app != "" {
		req.AppName = &app
	}
	stream, err := conn.TelemetryService.StreamLogs(streamCtx, req)
	if err != nil {
		stop()
		return err
	}
	session := b.session
	go func() {
		defer stop()
		for {
			v, err := stream.Recv()
			if err != nil {
				if streamCtx.Err() == nil {
					b.emitText(streamCtx, EventLogsError, session, streamID, err.Error())
				}
				return
			}
			for _, resource := range v.GetLogs().GetResourceLogs() {
				for _, scope := range resource.GetScopeLogs() {
					for _, record := range scope.GetLogRecords() {
						b.emit(streamCtx, Event{Kind: EventLog, Session: session, Stream: streamID, Log: desktopLog(record, resource.GetResource().GetAttributes())})
					}
				}
			}
		}
	}()
	return nil
}
func (b *Backend) StartCamera(camera uint32, streamID, sessionID uint64, expectedStableID ...string) error {
	_, done := b.begin()
	defer done()
	if err := b.validSession(sessionID); err != nil {
		return err
	}
	if len(expectedStableID) > 0 && expectedStableID[0] != "" && b.cameraDevices[camera].GetStableId() != expectedStableID[0] {
		return errors.New("camera identity changed; refresh cameras before starting video")
	}
	if b.lite != nil {
		return b.startLiteCamera(camera, streamID)
	}
	conn, err := b.connection(sessionID)
	if err != nil {
		return err
	}
	if subscription, ok := b.videoStops[camera]; ok {
		subscription.stop()
	}
	if b.videoStops == nil {
		b.videoStops = make(map[uint32]cameraSubscription)
	}
	streamCtx, stop := context.WithCancel(b.life)
	b.videoStops[camera] = cameraSubscription{stream: streamID, stop: stop}
	device := b.cameraDevices[camera]
	thermalProfile := thermal.CameraProfile(device)
	isThermal := thermalProfile != nil
	request := &pb.StreamVideoRequest{DeviceId: camera, StableId: device.GetStableId(), Codec: pb.VideoCodec_VIDEO_CODEC_H264}
	if isThermal {
		request.Codec = pb.VideoCodec_VIDEO_CODEC_RAW
		request.Width, request.Height = thermalProfile.CaptureWidth, thermalProfile.CaptureHeight
	}
	stream, err := conn.VideoService.StreamVideo(streamCtx, request)
	if err != nil {
		stop()
		return err
	}
	session := b.session
	timeout := b.cameraFirstFrameTimeout
	go func() {
		defer stop()
		firstFrame := time.AfterFunc(timeout, func() {
			b.emitText(streamCtx, EventCameraError, session, streamID, "This camera is not sending video. Check its source, or choose another camera.")
			stop()
		})
		defer firstFrame.Stop()
		for {
			v, err := stream.Recv()
			if err != nil {
				if streamCtx.Err() == nil {
					b.emitText(streamCtx, EventCameraError, session, streamID, err.Error())
				}
				return
			}
			if len(v.Data) == 0 {
				continue
			}
			if isThermal {
				measured, err := decodeThermalFrame(v, thermalProfile)
				if err != nil {
					b.emitText(streamCtx, EventCameraError, session, streamID, err.Error())
					return
				}
				firstFrame.Reset(5 * time.Second)
				b.emit(streamCtx, Event{Kind: EventThermal, Session: session, Stream: streamID, Bytes: measured.RGBA, Thermal: measured})
				continue
			}
			firstFrame.Stop()
			if v.GetCodec() != pb.VideoCodec_VIDEO_CODEC_H264 {
				b.emitText(streamCtx, EventCameraError, session, streamID, "This camera did not provide H.264 video.")
				return
			}
			if len(v.Data) > 16<<20 {
				b.emitText(streamCtx, EventCameraError, session, streamID, "Camera chunk exceeds 16 MB.")
				return
			}
			b.emit(streamCtx, Event{Kind: EventVideo, Session: session, Stream: streamID, Bytes: v.Data})
		}
	}()
	return nil
}

func (b *Backend) Account() (commands.DesktopAccountInfo, error) {
	_, done := b.begin()
	defer done()
	return commands.DesktopAccount()
}

func (b *Backend) Accounts() ([]commands.DesktopAccountInfo, error) {
	_, done := b.begin()
	defer done()
	return commands.DesktopAccounts()
}
