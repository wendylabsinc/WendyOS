package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

type liteConnection interface {
	GetDeviceInfo(time.Duration) (*liteclient.DeviceInfo, error)
	GetSensorManifest(time.Duration) (*sensorlinkpb.SensorManifest, error)
	SensorLinkSubscribe([]uint32, time.Duration) error
	SensorLinkUnsubscribe([]uint32, time.Duration) error
	AddSensorDataListener(func(*sensorlinkpb.SensorData)) func()
	ConsoleAttach(bool, bool) (<-chan liteclient.ConsoleChunk, func(bool) error, error)
	StopApp() error
	PushConf(*litepb.WendyConf, liteclient.ConfPushMode, func(uint32, uint32)) error
	ResetTargetDevice(bool, time.Duration) error
	Done() <-chan struct{}
	Close() error
}

func connectDesktopTarget(ctx context.Context, selector string) (*grpcclient.AgentConnection, liteConnection, error) {
	agent, lite, err := commands.DesktopConnectTarget(ctx, selector)
	if lite == nil {
		return agent, nil, err
	}
	return agent, lite, err
}

func (b *Backend) validSession(session uint64) error {
	if session != 0 && session != b.session {
		return errors.New("device session changed")
	}
	if b.conn == nil && b.lite == nil {
		return errors.New("connect to a device first")
	}
	return nil
}

func (b *Backend) IsLite(session uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return session == b.session && b.lite != nil
}

func (b *Backend) liteCameras() ([]*pb.VideoDevice, error) {
	manifest, err := b.lite.GetSensorManifest(3 * time.Second)
	if err != nil {
		return nil, err
	}
	result := []*pb.VideoDevice{}
	for _, channel := range manifest.GetSensors() {
		if channel.GetVideo() != nil {
			result = append(result, &pb.VideoDevice{Id: channel.GetChannelId(), Name: channel.GetName(), Path: fmt.Sprintf("SensorLink channel %d", channel.GetChannelId())})
		}
	}
	return result, nil
}

// Called with b.mu held. The CLI stream never blocks WendyCom's reader.
func (b *Backend) startLiteCamera(camera uint32, streamID uint64) error {
	manifest, err := b.lite.GetSensorManifest(3 * time.Second)
	if err != nil {
		return err
	}
	var selected *sensorlinkpb.SensorDescriptor
	for _, channel := range manifest.GetSensors() {
		if channel.GetChannelId() == camera && channel.GetVideo() != nil {
			selected = channel
			break
		}
	}
	if selected == nil {
		return errors.New("SensorLink camera was not found")
	}
	// Cancel and wait for the previous subscription to release this channel
	// before subscribing again, so its cleanup cannot unsubscribe the new stream.
	if previous, ok := b.videoStops[camera]; ok {
		previous.stop()
		if previous.done != nil {
			<-previous.done
		}
	}
	ctx, cancel := context.WithCancel(b.life)
	stream, err := commands.DesktopSubscribeLiteCamera(ctx, b.lite, selected)
	if err != nil {
		cancel()
		return err
	}
	if b.videoStops == nil {
		b.videoStops = make(map[uint32]cameraSubscription)
	}
	finished := make(chan struct{})
	b.videoStops[camera] = cameraSubscription{stream: streamID, stop: cancel, done: finished}
	kind := EventVideo
	if selected.GetVideo().GetCodec() == sensorlinkpb.VideoFormat_MJPEG {
		kind = EventJPEG
	}
	session := b.session
	timeout := b.cameraFirstFrameTimeout
	go func() {
		defer close(finished)
		defer stream.Close()
		defer cancel()
		first := time.AfterFunc(timeout, func() {
			b.emitText(ctx, EventCameraError, session, streamID, "This camera is not sending video. Check its source, or choose another camera.")
			cancel()
		})
		defer first.Stop()
		for {
			frame, err := stream.Recv()
			if err != nil {
				if ctx.Err() == nil {
					b.emitText(ctx, EventCameraError, session, streamID, err.Error())
				}
				return
			}
			first.Stop()
			b.emit(ctx, Event{Kind: kind, Session: session, Stream: streamID, Bytes: frame.Payload})
		}
	}()
	return nil
}

func (b *Backend) startLiteLogs(app string, streamID uint64) error {
	if app != "" {
		return errors.New("Wendy Lite exposes one device console; clear the app filter")
	}
	if b.logsStop != nil {
		b.logsStop()
	}
	if b.logsDone != nil {
		<-b.logsDone
	}
	chunks, detach, err := b.lite.ConsoleAttach(true, false)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(b.life)
	b.logsStop = cancel
	finished := make(chan struct{})
	b.logsDone = finished
	session := b.session
	go func() {
		defer close(finished)
		defer cancel()
		defer detach(false)
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-chunks:
				if !ok {
					if ctx.Err() == nil {
						b.emitText(ctx, EventLogsError, session, streamID, "The Wendy Lite console disconnected.")
					}
					return
				}
				if chunk.Gap {
					b.emit(ctx, Event{Kind: EventLog, Session: session, Stream: streamID, Log: &LogRecord{Level: LogWarn, Body: "Some console output was lost."}})
				}
				if len(chunk.Data) > 0 {
					level := LogInfo
					if chunk.Stderr {
						level = LogError
					}
					b.emit(ctx, Event{Kind: EventLog, Session: session, Stream: streamID, Log: &LogRecord{Level: level, Body: string(chunk.Data)}})
				}
			}
		}
	}()
	return nil
}

func (b *Backend) joinLiteWiFi(ssid, password string) error {
	if strings.TrimSpace(ssid) == "" || len([]byte(ssid)) > 32 {
		return errors.New("enter a Wi-Fi network name of 1–32 bytes")
	}
	// Matches the CLI Lite Wi-Fi operation: replace the Wi-Fi field only, then reboot.
	_ = b.lite.StopApp()
	conf := &litepb.WendyConf{Wifi: &litepb.WendyConfWifi{Networks: []*litepb.WendyConfWifiNetwork{{Ssid: ssid, Password: password}}}}
	if err := b.lite.PushConf(conf, liteclient.ConfPushModeUpdate, nil); err != nil {
		return err
	}
	return b.lite.ResetTargetDevice(true, 0)
}
