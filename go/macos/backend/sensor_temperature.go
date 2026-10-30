package backend

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

type SensorTemperature struct {
	Name        string
	Celsius     float64
	TimestampUs uint64
	Sequence    uint32
}

// SensorTemperature takes one fresh sample over the current authenticated
// WendyCom connection. Each bounded read releases its subscription before
// returning; it owns no producer or listener after its view disappears.
func (b *Backend) SensorTemperature(session uint64) (SensorTemperature, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return SensorTemperature{}, errors.New("select a device first")
	}
	if err := b.validSession(session); err != nil {
		return SensorTemperature{}, err
	}
	if b.lite == nil {
		return SensorTemperature{}, errors.New("select a Wendy Lite SensorLink source")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	return readSensorTemperature(ctx, b.lite)
}

func readSensorTemperature(ctx context.Context, client liteConnection) (sample SensorTemperature, err error) {
	if err := ctx.Err(); err != nil {
		return sample, err
	}
	manifest, err := client.GetSensorManifest(3 * time.Second)
	if err != nil {
		return sample, fmt.Errorf("SensorLink manifest: %w", err)
	}
	var selected *sensorlinkpb.SensorDescriptor
	for _, channel := range manifest.GetSensors() {
		if channel.GetSensor().GetSchema() == "wendy.temperature.celsius.f32le.v1" && channel.GetSensor().GetSampleBytes() == 4 {
			selected = channel
			break
		}
	}
	if selected == nil {
		return sample, errors.New("this device does not advertise a SensorLink temperature channel")
	}
	frames := make(chan *sensorlinkpb.SensorData, 1)
	remove := client.AddSensorDataListener(func(frame *sensorlinkpb.SensorData) {
		if frame.GetChannelId() != selected.GetChannelId() {
			return
		}
		copy := &sensorlinkpb.SensorData{ChannelId: frame.GetChannelId(), FrameSeq: frame.GetFrameSeq(), ChunkSeq: frame.GetChunkSeq(), TsUs: frame.GetTsUs(), Flags: frame.GetFlags()}
		if len(frame.GetPayload()) == 4 {
			copy.Payload = append([]byte(nil), frame.GetPayload()...)
		}
		select {
		case frames <- copy:
		default:
		}
	})
	defer remove()
	channels := []uint32{selected.GetChannelId()}
	// An acknowledgement may be lost after the source accepted Subscribe.
	// Retire the subscription even on an uncertain setup/cancellation result.
	defer func() {
		if cleanup := client.SensorLinkUnsubscribe(channels, 2*time.Second); cleanup != nil {
			// A lost stop acknowledgement leaves producer ownership uncertain.
			// Closing WendyCom retires that subscription at the source and
			// lets normal connection monitoring report the lost session.
			_ = client.Close()
			err = errors.Join(err, fmt.Errorf("SensorLink unsubscribe: %w", cleanup))
		}
	}()
	if err := client.SensorLinkSubscribe(channels, 3*time.Second); err != nil {
		return sample, fmt.Errorf("SensorLink subscribe: %w", err)
	}
	select {
	case frame := <-frames:
		if frame.GetChunkSeq() != 0 || frame.GetFlags()&2 == 0 || len(frame.GetPayload()) != 4 || frame.GetTsUs() == 0 {
			return sample, errors.New("invalid SensorLink temperature sample")
		}
		value := float64(math.Float32frombits(binary.LittleEndian.Uint32(frame.GetPayload())))
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return sample, errors.New("invalid SensorLink temperature value")
		}
		return SensorTemperature{Name: selected.GetName(), Celsius: value, TimestampUs: frame.GetTsUs(), Sequence: frame.GetFrameSeq()}, nil
	case <-ctx.Done():
		return sample, ctx.Err()
	case <-client.Done():
		return sample, errors.New("SensorLink connection closed before a sample arrived")
	}
}
