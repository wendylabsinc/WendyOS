package sensorrecord

import (
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/livecapture"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// Publication builds typed submission metadata without any caller-selected
// source, epoch or publication sequence. It does not upload or admit evidence.
func Publication(c Capture, ttl time.Duration) (serviceorigin.PublishRequest, error) {
	raw, err := Encode(c)
	if err != nil {
		return serviceorigin.PublishRequest{}, err
	}
	s, _ := sample(c.SampleJSON)
	domain := "simulation:" + s.RunID
	p := serviceorigin.PublishRequest{
		Entity: "sensor/" + s.SensorID, ComponentType: ComponentType, Stream: s.SensorID,
		Frame: s.Frame, TTL: ttl, Payload: raw,
		Timing: world.Timing{SampleTime: time.Unix(0, c.SimulationAnchorUnixNS+s.SampleNS).UTC(), ClockDomain: domain, ErrorKnown: true},
		Provenance: world.Provenance{Origin: world.Simulated, AppVersion: AppVersion, ModelID: s.SensorModel, ModelVersion: s.ScenarioVersion,
			Simulation: &world.SimulationContext{RunID: s.RunID, ScenarioVersion: s.ScenarioVersion, Seed: s.Seed, ClockDomain: domain}},
	}
	_, err = ValidatePublication(p)
	return p, err
}

// FromSnapshot converts one selected sensor from a coherent live capture. The
// caller supplies the commissioned fixed anchor and obtains current-age and
// cross-response boot/ordering checks from livecapture.Client. Revalidation here
// uses the captured wall instant only to check snapshot coherence, not freshness.
// Snapshot has already decoded sample JSON, so only this conversion re-encodes
// it; callers with original JSON bytes should construct Capture directly.
func FromSnapshot(snapshot livecapture.Snapshot, sensorID string, anchor time.Time, ttl time.Duration) (serviceorigin.PublishRequest, []byte, error) {
	if anchor.IsZero() || anchor.UnixNano() <= 0 || !time.Unix(0, anchor.UnixNano()).Equal(anchor) || snapshot.SampleUnixMS <= 0 || snapshot.SampleUnixMS > math.MaxInt64/int64(time.Millisecond) || len(snapshot.Observations) > livecapture.MaxObservations || len(snapshot.Media) > 3 {
		return serviceorigin.PublishRequest{}, nil, errors.New("invalid bounded snapshot or simulation anchor")
	}
	totalBytes := 0
	for _, media := range snapshot.Media {
		if len(media.Data) > livecapture.MaxResponseBytes-totalBytes {
			return serviceorigin.PublishRequest{}, nil, errors.New("snapshot media exceeds response budget")
		}
		totalBytes += len(media.Data)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return serviceorigin.PublishRequest{}, nil, err
	}
	captured := time.UnixMilli(snapshot.SampleUnixMS)
	checked, err := livecapture.Decode(raw, livecapture.Config{RunID: snapshot.RunID, Site: snapshot.State.SiteID, Frame: snapshot.State.Frame, Now: func() time.Time { return captured }, MaxAge: 500 * time.Millisecond})
	if err != nil {
		return serviceorigin.PublishRequest{}, nil, err
	}
	for _, selected := range checked.Observations {
		if selected.SensorID != sensorID {
			continue
		}
		sampleJSON, err := json.Marshal(selected)
		if err != nil {
			return serviceorigin.PublishRequest{}, nil, err
		}
		capture := Capture{Version: Version, SceneBootID: checked.BootID, CapturedUnixNS: checked.SampleUnixMS * int64(time.Millisecond), SimulationAnchorUnixNS: anchor.UnixNano(), SampleJSON: sampleJSON}
		request, err := Publication(capture, ttl)
		if err != nil {
			return request, nil, err
		}
		var media []byte
		if selected.Evidence != nil {
			media, err = checked.Evidence(*selected.Evidence)
			if err != nil {
				return request, nil, err
			}
		}
		if err = ValidateMedia(capture, media); err != nil {
			return request, nil, err
		}
		return request, media, nil
	}
	return serviceorigin.PublishRequest{}, nil, errors.New("requested sensor absent from capture")
}

// SensorLinkFrame contains only a completed parser result. The ordinary wire
// adapter supplies it; the agent metadata package must not import a transport
// implementation or register a second copy of Wendy's SensorLink protobufs.
type SensorLinkFrame struct {
	ChannelID uint32
	Seq       uint32
	TsUs      uint64
	Flags     uint32
	Payload   []byte
}

// BindSensorLink validates a reassembled received frame and binds its exact
// bytes to an original rendered capture. Native, wire and agent sequences are
// distinct. SensorLink framing authenticates neither the source nor the frame.
func BindSensorLink(c Capture, frame SensorLinkFrame) (Capture, error) {
	if c.SensorLink != nil || len(frame.Payload) < 1 || len(frame.Payload) > sensing.MaxEvidenceBytes {
		return Capture{}, errors.New("capture already bound or frame bytes outside bounds")
	}
	s, err := c.Sample()
	if err != nil {
		return Capture{}, err
	}
	calibration, err := CalibrationDigest(s)
	if err != nil {
		return Capture{}, err
	}
	c.SampleJSON = append([]byte(nil), c.SampleJSON...)
	c.SensorLink = &WireBinding{Protocol: "wendylite.v1.sensorlink", ChannelID: frame.ChannelID, FrameSequence: frame.Seq, TimestampUS: frame.TsUs, Flags: frame.Flags, SceneBootID: c.SceneBootID, CaptureSequence: s.Sequence, CapturedUnixNS: c.CapturedUnixNS, SampleSHA256: digest(c.SampleJSON), CalibrationSHA256: calibration, Evidence: sensing.EvidenceRef{SHA256: digest(frame.Payload), ContentType: "image/jpeg", Size: int64(len(frame.Payload))}}
	if err = ValidateMedia(c, frame.Payload); err != nil {
		return Capture{}, err
	}
	if _, err = Encode(c); err != nil {
		return Capture{}, err
	}
	return c, nil
}

// ValidateMedia checks the selected evidence's digest, size, content type and
// actual decoded image dimensions or PCM audio interval. Gas and LiDAR accept no bytes.
func ValidateMedia(c Capture, data []byte) error {
	s, err := c.NormalizedSample()
	if err != nil {
		return err
	}
	if s.Evidence == nil {
		if len(data) != 0 {
			return errors.New("inline capture cannot carry media bytes")
		}
		return nil
	}
	ref := s.Evidence
	if len(data) > sensing.MaxEvidenceBytes || int64(len(data)) != ref.Size || digest(data) != ref.SHA256 {
		return errors.New("capture media digest or size mismatch")
	}
	if s.Camera != nil {
		err = validateImage(data, *s.Camera, ref.ContentType)
	} else if s.Audio != nil {
		err = validateAudio(data, *s.Audio)
	} else {
		err = errors.New("capture media has no interpretation metadata")
	}
	return err
}
