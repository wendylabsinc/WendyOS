// Package backend is the process-local Go implementation linked by SwiftUI.
package backend

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

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
)

type Event struct {
	Kind            EventKind
	Session, Stream uint64
	Text            string
	Bytes           []byte
	Log             *LogRecord
}
type Backend struct {
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
	connectDevice           func(context.Context, string) (*grpcclient.AgentConnection, error)
	unpinDevice             func(string) error
	cameraFirstFrameTimeout time.Duration
	// Operations are serialized. Stream producers communicate only through events.
	mu                  sync.Mutex
	conn                *grpcclient.AgentConnection
	life                context.Context
	stop                context.CancelFunc
	logsStop, loginStop context.CancelFunc
	videoStops          map[uint32]cameraSubscription
	session             uint64
	events              chan Event
	relay               *vpn.Relay
}

func New() *Backend {
	return &Backend{events: make(chan Event, 64), cameraFirstFrameTimeout: 15 * time.Second,
		connectDevice: commands.DesktopConnect, unpinDevice: commands.DesktopUnpin}
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
}

func (b *Backend) stopCameras() {
	for id, subscription := range b.videoStops {
		subscription.stop()
		delete(b.videoStops, id)
	}
}
func (b *Backend) disconnect() {
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
	b.stopCameras()
	if b.stop != nil {
		b.stop()
		b.stop = nil
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
	if b.conn == nil {
		return nil, errors.New("connect to a device first")
	}
	if session != 0 && session != b.session {
		return nil, errors.New("device session changed")
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
	b.mu.Unlock()

	// Keep the current connection and its streams alive while dialing.
	conn, err := b.connectDevice(ctx, selector)
	if err == nil {
		_, err = conn.AgentService.GetAgentVersion(ctx, &pb.GetAgentVersionRequest{})
		if err != nil {
			conn.Close()
			conn = nil
		}
	}

	b.mu.Lock()
	b.connectMu.Lock()
	if request != b.connectSequence || ctx.Err() != nil {
		sameRequest := request == b.connectSequence
		b.connectMu.Unlock()
		b.mu.Unlock()
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
	previousConnection := b.conn
	b.conn = nil
	b.disconnect()
	b.conn = conn
	b.deviceSelector = selector
	b.life, b.stop = context.WithCancel(context.Background())
	session := b.session
	b.connectMu.Unlock()
	b.mu.Unlock()
	// A tunnel's Close may wait for transport shutdown; never hold UI control
	// or device-operation locks while doing that work.
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
	if b.relay != nil {
		b.relay.Close()
		b.relay = nil
	}
	relay, config, err := vpn.NewRelay(routes, desktopDial)
	if err != nil {
		return vpn.Config{}, err
	}
	b.relay = relay
	return config, nil
}
func (b *Backend) StopVPN() error {
	_, done := b.begin()
	defer done()
	if b.relay != nil {
		b.relay.Close()
		b.relay = nil
	}
	return nil
}
func (b *Backend) Shutdown() error {
	b.ReserveConnection()
	_, done := b.begin()
	defer done()
	b.disconnect()
	if b.loginStop != nil {
		b.loginStop()
	}
	if b.relay != nil {
		b.relay.Close()
		b.relay = nil
	}
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
	conn, err := b.connection(0)
	if err != nil {
		return nil, err
	}
	v, err := conn.VideoService.ListVideoDevices(ctx, &pb.ListVideoDevicesRequest{})
	if err != nil {
		return nil, err
	}
	return v.Devices, nil
}
func (b *Backend) StopLogs() error {
	_, done := b.begin()
	defer done()
	_, err := b.connection(0)
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
	_, err := b.connection(sessionID)
	if err != nil {
		return err
	}
	if subscription, ok := b.videoStops[camera]; ok && (streamID == 0 || subscription.stream == streamID) {
		subscription.stop()
		delete(b.videoStops, camera)
	}
	return nil
}
func (b *Backend) StopCameras(sessionID uint64) error {
	_, done := b.begin()
	defer done()
	_, err := b.connection(sessionID)
	if err != nil {
		return err
	}
	b.stopCameras()
	return nil
}
func (b *Backend) StartLogs(app string, streamID uint64) error {
	_, done := b.begin()
	defer done()
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
func (b *Backend) StartCamera(camera uint32, streamID, sessionID uint64) error {
	_, done := b.begin()
	defer done()
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
	stream, err := conn.VideoService.StreamVideo(streamCtx, &pb.StreamVideoRequest{DeviceId: camera, Codec: pb.VideoCodec_VIDEO_CODEC_H264})
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
