package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy-go/lite"
	sdkpb "github.com/wendylabsinc/wendy-go/lite/pb"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type liteAudioOptions struct {
	id, sampleRate, channels, bufferMs  uint32
	idSet, rateSet, channelsSet, stdout bool
}

func liteAudioChannels(manifest *sensorlinkpb.SensorManifest) []*sensorlinkpb.SensorDescriptor {
	converted := new(sdkpb.SensorManifest)
	_ = liteAudioMessage(manifest, converted)
	var out []*sensorlinkpb.SensorDescriptor
	for _, ch := range lite.AudioChannels(converted) {
		m := new(sensorlinkpb.SensorDescriptor)
		_ = liteAudioMessage(ch, m)
		out = append(out, m)
	}
	return out
}
func listLiteAudio(cmd *cobra.Command, target *SelectedDevice) error {
	// Camera and audio use the same authenticated SensorLink transport.
	client, closeClient, err := openLiteCamera(cmd.Context(), target)
	if err != nil {
		return err
	}
	defer closeClient()
	manifest, err := client.GetSensorManifest(3 * time.Second)
	if err != nil {
		return fmt.Errorf("listing SensorLink microphones: %w", err)
	}
	channels := liteAudioChannels(manifest)
	if jsonOutput {
		rows := make([]json.RawMessage, 0, len(channels))
		marshal := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}
		for _, ch := range channels {
			b, err := marshal.Marshal(ch)
			if err != nil {
				return err
			}
			rows = append(rows, b)
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(channels) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No microphones found.")
		return nil
	}
	var rows [][]string
	for _, ch := range channels {
		a := ch.GetAudio()
		rows = append(rows, []string{fmt.Sprint(ch.GetChannelId()), ch.GetName(), a.GetCodec().String(), fmt.Sprint(a.GetSampleRate()), fmt.Sprint(a.GetChannels())})
	}
	fmt.Fprint(cmd.OutOrStdout(), tui.RenderTable([]string{"ID", "Name", "Codec", "Sample rate", "Channels"}, rows))
	return nil
}

func selectLiteAudio(manifest *sensorlinkpb.SensorManifest, opts liteAudioOptions) (*sensorlinkpb.SensorDescriptor, error) {
	converted := new(sdkpb.SensorManifest)
	if err := liteAudioMessage(manifest, converted); err != nil {
		return nil, err
	}
	ch, err := lite.SelectAudio(converted, lite.AudioOptions{ID: opts.id, SampleRate: opts.sampleRate, Channels: opts.channels, IDSet: opts.idSet, RateSet: opts.rateSet, ChannelsSet: opts.channelsSet})
	if err != nil {
		return nil, err
	}
	out := new(sensorlinkpb.SensorDescriptor)
	err = liteAudioMessage(ch, out)
	return out, err
}
func listenLiteAudio(cmd *cobra.Command, target *SelectedDevice, opts liteAudioOptions) error {
	ctx := cmd.Context()
	client, closeClient, err := openLiteCamera(ctx, target)
	if err != nil {
		return err
	}
	defer closeClient()
	manifest, err := client.GetSensorManifest(3 * time.Second)
	if err != nil {
		return fmt.Errorf("listing SensorLink microphones: %w", err)
	}
	ch, err := selectLiteAudio(manifest, opts)
	if err != nil {
		return err
	}
	stream, err := subscribeLiteAudio(ctx, client, ch)
	if err != nil {
		return err
	}
	defer stream.Close()
	a := ch.GetAudio()
	fmt.Fprintf(cmd.ErrOrStderr(), "Streaming %s, SensorLink channel %d (%d Hz, %d channels, PCM S16LE). Ctrl+C to stop.\n", ch.GetName(), ch.GetChannelId(), a.GetSampleRate(), a.GetChannels())
	if !opts.stdout {
		err = playRealtimeAudio(ctx, stream, a.GetSampleRate(), a.GetChannels(), opts.bufferMs)
	} else {
		for {
			var chunk *agentpb.AudioChunk
			chunk, err = stream.Recv()
			if err != nil {
				break
			}
			var n int
			n, err = cmd.OutOrStdout().Write(chunk.GetPcmData())
			if err == nil && n != len(chunk.GetPcmData()) {
				err = io.ErrShortWrite
			}
			if err != nil {
				break
			}
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// liteAudioStream projects Core PCM chunks into the existing playback interface.
type liteAudioStream struct{ *lite.AudioStream }

func subscribeLiteAudio(ctx context.Context, client liteCameraClient, ch *sensorlinkpb.SensorDescriptor) (*liteAudioStream, error) {
	converted := new(sdkpb.SensorDescriptor)
	if err := liteAudioMessage(ch, converted); err != nil {
		return nil, err
	}
	stream, err := lite.SubscribeAudio(ctx, liteAudioClient{client}, converted)
	if err != nil {
		return nil, err
	}
	return &liteAudioStream{stream}, nil
}
func (s *liteAudioStream) Recv() (*agentpb.AudioChunk, error) {
	m, err := s.AudioStream.Recv()
	if err != nil {
		return nil, err
	}
	return &agentpb.AudioChunk{PcmData: m.PcmData, TimestampNs: m.TimestampNs, SampleRate: m.SampleRate, Channels: m.Channels}, nil
}

type liteAudioClient struct{ liteCameraClient }

func (c liteAudioClient) AddSensorDataListener(fn func(*sdkpb.SensorData)) func() {
	return c.liteCameraClient.AddSensorDataListener(func(m *sensorlinkpb.SensorData) { out := new(sdkpb.SensorData); _ = liteAudioMessage(m, out); fn(out) })
}
func liteAudioMessage(src, dst proto.Message) error {
	data, err := proto.Marshal(src)
	if err != nil {
		return err
	}
	return proto.Unmarshal(data, dst)
}
