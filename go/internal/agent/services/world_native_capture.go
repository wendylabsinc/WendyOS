package services

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"github.com/wendylabsinc/wendy/go/world/production/cameraframe"
)

var errWorldNativeCamera = errors.New("native camera frame unavailable")

type worldNativeCameraSource struct {
	ctx      context.Context
	allowed  map[string]bool
	metadata func(context.Context, string) ([]byte, error)
	receipt  func() (cameraframe.Receipt, error)
	session  func() (string, error)
	mu       sync.Mutex
	latest   map[string]cameraframe.Frame
}

// StartNativeCapture uses the existing paired native camera stream. Only
// commissioned source IDs retain bytes; it opens no source or app listener.
func (m *AppWorldSocketManager) StartNativeCapture(supervisor *mcusource.Supervisor, metadata *CalibratedFrameService) error {
	if m == nil || m.module == nil || supervisor == nil || metadata == nil {
		return errWorldNativeCamera
	}
	ids := m.module.NativeCameraSourceIDs()
	if len(ids) == 0 {
		return nil
	}
	bootID := data.BootID()
	source, err := newWorldNativeCameraSource(m.ctx, ids,
		func(ctx context.Context, id string) ([]byte, error) {
			response, err := metadata.ListCameraMetadata(ctx, &agentpbv2.ListCameraMetadataRequest{StableIds: []string{id}})
			if err != nil || response == nil {
				return nil, errWorldNativeCamera
			}
			if len(response.Cameras) == 0 {
				return nil, nil // Missing calibration is explicit, never fabricated.
			}
			if len(response.Cameras) != 1 || response.Cameras[0].GetStableId() != id {
				return nil, errWorldNativeCamera
			}
			return json.Marshal(response.Cameras[0])
		}, func() (cameraframe.Receipt, error) {
			if bootID == "" || bootID == "unavailable" {
				return cameraframe.Receipt{}, errWorldNativeCamera
			}
			before, received, after, err := data.CaptureReceipt()
			return cameraframe.Receipt{BootID: bootID, BeforeBootNS: before, ReceivedBootNS: received, AfterBootNS: after}, err
		})
	if err != nil {
		return err
	}
	if err = m.module.SetNativeCameraProvider(source.ReadFrame); err != nil {
		return err
	}
	supervisor.SetCameraFrameObserver(source.ObserveStream, source.WantsChannel)
	return nil
}

func newWorldNativeCameraSource(ctx context.Context, ids []string, metadata func(context.Context, string) ([]byte, error), receipt func() (cameraframe.Receipt, error)) (*worldNativeCameraSource, error) {
	if ctx == nil || metadata == nil || receipt == nil || len(ids) == 0 || len(ids) > 16 {
		return nil, errWorldNativeCamera
	}
	s := &worldNativeCameraSource{ctx: ctx, allowed: make(map[string]bool), metadata: metadata, receipt: receipt, latest: make(map[string]cameraframe.Frame)}
	for _, id := range ids {
		// Pairing keys themselves contain colons. Only the final channel is
		// numeric; the complete stable ID remains the exact commissioned key.
		last := strings.LastIndexByte(id, ':')
		if !strings.HasPrefix(id, "sensorlink:") || last <= len("sensorlink:") || s.allowed[id] {
			return nil, errWorldNativeCamera
		}
		n, err := strconv.ParseUint(id[last+1:], 10, 32)
		if err != nil || strconv.FormatUint(n, 10) != id[last+1:] {
			return nil, errWorldNativeCamera
		}
		s.allowed[id] = true
	}
	s.session = func() (string, error) {
		var value [16]byte
		if _, err := rand.Read(value[:]); err != nil {
			return "", err
		}
		return hex.EncodeToString(value[:]), nil
	}
	return s, nil
}

func (s *worldNativeCameraSource) ObserveStream(ctx context.Context, pairing mcusource.SensorPairing) (func(*sensorlink.SensorFrame, ros2camera.Frame), func()) {
	if ctx == nil || ctx.Err() != nil || s.ctx.Err() != nil {
		return nil, nil
	}
	session, err := s.session()
	if err != nil {
		return nil, nil
	}
	var closed bool
	observe := func(native *sensorlink.SensorFrame, format ros2camera.Frame) {
		if native == nil || ctx.Err() != nil || s.ctx.Err() != nil {
			return
		}
		id := "sensorlink:" + pairing.Key() + ":" + strconv.FormatUint(uint64(native.ChannelID), 10)
		if !s.allowed[id] {
			return
		}
		// This is gateway receipt after native frame reassembly, not MCU
		// capture time. TsUs has no clock basis in the native wire contract.
		received, err := s.receipt()
		if err != nil || format.Codec != ros2camera.CodecMJPEG || !bytes.Equal(format.Data, native.Payload) {
			s.remove(id, session)
			return
		}
		calibration, err := s.metadata(ctx, id)
		if err != nil {
			s.remove(id, session)
			return
		}
		transport := pairing.Transport
		if transport == "" {
			transport = "tcp"
		}
		meta := cameraframe.Metadata{Version: 1, SourceID: id, SourceSessionID: session, Transport: transport,
			ChannelID: native.ChannelID, FrameSequence: native.Seq, Flags: native.Flags,
			TimestampUS: native.TsUs, TimestampBasis: "unknown", Receipt: received,
			Codec: "mjpeg", Width: format.Width, Height: format.Height, CalibrationJSON: calibration}
		frame, err := cameraframe.NewFrame(meta, native.Payload)
		if err != nil {
			s.remove(id, session)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if !closed && ctx.Err() == nil && s.ctx.Err() == nil {
			s.latest[id] = frame
		}
	}
	close := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		closed = true
		for id, frame := range s.latest {
			if frame.Metadata.SourceSessionID == session {
				delete(s.latest, id)
			}
		}
	}
	return observe, close
}

func (s *worldNativeCameraSource) WantsChannel(pairing mcusource.SensorPairing, channel uint32) bool {
	return s.ctx.Err() == nil && s.allowed["sensorlink:"+pairing.Key()+":"+strconv.FormatUint(uint64(channel), 10)]
}

func (s *worldNativeCameraSource) remove(id, session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest[id].Metadata.SourceSessionID == session {
		delete(s.latest, id)
	}
}

func (s *worldNativeCameraSource) ReadFrame(ctx context.Context, sourceID string) (cameraframe.Frame, error) {
	if ctx == nil || ctx.Err() != nil || s.ctx.Err() != nil || !s.allowed[sourceID] {
		return cameraframe.Frame{}, errWorldNativeCamera
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	frame, ok := s.latest[sourceID]
	if !ok {
		return cameraframe.Frame{}, errWorldNativeCamera
	}
	// The app stream owns this snapshot even if the next native frame arrives.
	return frame.Clone(), nil
}
