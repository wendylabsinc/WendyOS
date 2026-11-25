// Package sensorlinkbridge maps explicitly configured simulation MJPEG streams
// into World observations. Raw TCP does not authenticate a physical camera.
package sensorlinkbridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"math"
	"net"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/sensorlink"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type Config struct {
	Publisher               client.Config  `json:"publisher"`
	Endpoint                string         `json:"endpoint"`
	ChannelID               uint32         `json:"channel_id"`
	SampleTemplate          sensing.Sample `json:"sample_template"`
	AnchorUnixUS            int64          `json:"anchor_unix_us"`
	TimestampClock          string         `json:"timestamp_clock"`
	ClockErrorUS            uint32         `json:"clock_error_us"`
	TTLMS                   uint32         `json:"ttl_ms"`
	AllowInsecureSimulation bool           `json:"allow_insecure_simulation"`
}

func Decode(raw []byte) (Config, error) {
	var cfg Config
	if err := authority.DecodeStrict(raw, &cfg); err != nil {
		return cfg, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return cfg, err
	}
	s, err := sensing.Decode(fields["sample_template"])
	if err != nil {
		return cfg, err
	}
	cfg.SampleTemplate = s
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	g, s := c.Publisher.Grant.Grant, c.SampleTemplate
	if !c.AllowInsecureSimulation || !c.Publisher.AllowInsecureSimulation || g.Domain != "simulation" || c.TimestampClock != "unix-us" || c.ChannelID == 0 || c.AnchorUnixUS <= 0 || c.AnchorUnixUS > math.MaxInt64/1000 || c.ClockErrorUS == 0 || c.ClockErrorUS > 100000 || c.TTLMS < 1 || c.TTLMS > 60000 || uint64(c.ClockErrorUS) >= uint64(c.TTLMS)*1000 {
		return errors.New("explicit simulation source, Unix-microsecond clock mapping and bounded lifetime required")
	}
	host, port, err := net.SplitHostPort(c.Endpoint)
	if err != nil || host == "" || port == "" {
		return errors.New("SensorLink endpoint must be an explicit host:port")
	}
	if err = s.Validate(); err != nil {
		return err
	}
	if s.Kind != "camera" || s.Camera == nil || s.SensorLink != nil || g.Tenant == "" || g.Site != s.SiteID || g.Source.Service != s.SensorID {
		return errors.New("camera template does not match authenticated publisher")
	}
	_, err = client.New(c.Publisher)
	return err
}

func (c Config) ValidateManifest(m *pb.SensorManifest) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if m == nil {
		return errors.New("missing SensorLink manifest")
	}
	if err := sensorlink.ValidateManifest(m, []uint32{c.ChannelID}); err != nil {
		return err
	}
	for _, d := range m.Sensors {
		if d.GetChannelId() != c.ChannelID {
			continue
		}
		v := d.GetVideo()
		if v == nil || v.Codec != pb.VideoFormat_MJPEG || int(v.Width) != c.SampleTemplate.Camera.Width || int(v.Height) != c.SampleTemplate.Camera.Height {
			return errors.New("selected SensorLink channel format differs from calibration")
		}
		return nil
	}
	return errors.New("selected SensorLink channel absent")
}

type Converter struct {
	cfg             Config
	epoch, sequence uint64
	seen            bool
	lastFrame       uint32
	lastUS          uint64
}

func New(c Config, epoch uint64) (*Converter, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if epoch == 0 {
		return nil, errors.New("publisher epoch required")
	}
	// Freeze the template and credentials to prevent later caller mutation.
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	c, err = Decode(raw)
	if err != nil {
		return nil, err
	}
	return &Converter{cfg: c, epoch: epoch}, nil
}

func (c *Converter) Convert(frame *sensorlink.Frame, now time.Time) (world.Component, error) {
	if frame == nil || frame.ChannelID != c.cfg.ChannelID || frame.TsUs == 0 || frame.TsUs > uint64(math.MaxInt64/1000) || frame.Flags & ^uint32(1) != 0 || len(frame.Payload) < 1 || len(frame.Payload) > sensing.MaxEvidenceBytes || now.IsZero() {
		return world.Component{}, errors.New("invalid SensorLink frame")
	}
	if c.seen {
		delta := frame.Seq - c.lastFrame
		if delta == 0 || delta >= 1<<31 || frame.TsUs <= c.lastUS {
			return world.Component{}, errors.New("SensorLink frame sequence or clock did not advance")
		}
	}
	stamp := time.UnixMicro(int64(frame.TsUs))
	errBound := time.Duration(c.cfg.ClockErrorUS) * time.Microsecond
	ttl := time.Duration(c.cfg.TTLMS) * time.Millisecond
	if stamp.After(now) || !now.Before(stamp.Add(ttl-errBound)) {
		return world.Component{}, errors.New("SensorLink capture is stale or future")
	}
	imageCfg, err := jpeg.DecodeConfig(bytes.NewReader(frame.Payload))
	if err != nil {
		return world.Component{}, fmt.Errorf("SensorLink JPEG header: %w", err)
	}
	s := c.cfg.SampleTemplate
	if imageCfg.Width != s.Camera.Width || imageCfg.Height != s.Camera.Height || imageCfg.Width < 1 || imageCfg.Height < 1 || imageCfg.Width > 1024 || imageCfg.Height > 1024 {
		return world.Component{}, errors.New("JPEG dimensions differ from selected calibration")
	}
	if _, err = jpeg.Decode(bytes.NewReader(frame.Payload)); err != nil {
		return world.Component{}, fmt.Errorf("SensorLink JPEG body: %w", err)
	}
	deltaUS := int64(frame.TsUs) - c.cfg.AnchorUnixUS
	if deltaUS < 0 || deltaUS > int64(24*time.Hour/time.Microsecond) {
		return world.Component{}, errors.New("SensorLink timestamp outside configured simulation run")
	}
	// The template calibration's original expiry stays fixed across frames.
	calUntil := time.UnixMicro(c.cfg.AnchorUnixUS).Add(time.Duration(s.SampleNS)).Add(time.Duration(s.Camera.CalibrationValidForNS))
	remaining := calUntil.Sub(stamp)
	if remaining <= errBound || !now.Before(calUntil.Add(-errBound)) {
		return world.Component{}, errors.New("configured camera calibration expired")
	}
	cam := *s.Camera
	cam.CameraToSite = append([]float64(nil), s.Camera.CameraToSite...)
	cam.CalibrationValidForNS = int64(remaining)
	s.Camera = &cam
	s.Sequence = c.sequence + 1
	s.ID = fmt.Sprintf("%s/%s/%d", s.RunID, s.SensorID, s.Sequence)
	s.SampleNS = deltaUS * 1000
	hash := sha256.Sum256(frame.Payload)
	digest := hex.EncodeToString(hash[:])
	s.Evidence = &sensing.EvidenceRef{SHA256: digest, ContentType: "image/jpeg", Size: int64(len(frame.Payload))}
	s.SensorLink = &sensing.SensorLink{Protocol: "wendylite.v1.sensorlink", ChannelID: frame.ChannelID, FrameSequence: frame.Seq, TimestampUS: frame.TsUs, Flags: frame.Flags, FrameSHA256: digest, SourceClock: c.cfg.TimestampClock, AnchorUnixUS: c.cfg.AnchorUnixUS, ClockErrorUS: c.cfg.ClockErrorUS}
	raw, err := json.Marshal(s)
	if err != nil {
		return world.Component{}, err
	}
	converter, err := sensing.New(sensing.Config{Tenant: c.cfg.Publisher.Grant.Grant.Tenant, Site: s.SiteID, RunID: s.RunID, Frame: s.Frame, Source: c.cfg.Publisher.Grant.Grant.Source, Epoch: c.epoch, Anchor: time.UnixMicro(c.cfg.AnchorUnixUS), TTL: ttl})
	if err != nil {
		return world.Component{}, err
	}
	component, err := converter.Convert(raw)
	if err != nil {
		return world.Component{}, err
	}
	component.Provenance.AppVersion = "world-sensorlink-v1"
	if _, err = sensing.FromComponent(component); err != nil {
		return world.Component{}, err
	}
	c.sequence = s.Sequence
	c.lastFrame = frame.Seq
	c.lastUS = frame.TsUs
	c.seen = true
	return component, nil
}

type Publisher interface {
	UploadEvidence(context.Context, []byte, string, time.Time) (evidence.PutResponse, error)
	DownloadEvidence(context.Context, string, int64) ([]byte, evidence.GetResponse, error)
	Publish(context.Context, world.Component) error
}

// Publish retains exact source JPEG bytes before publishing metadata. It never
// extends capture time, original TTL or another writer's evidence retention.
func Publish(ctx context.Context, p Publisher, component world.Component, data []byte, now func() time.Time) error {
	s, err := sensing.FromComponent(component)
	if err != nil {
		return err
	}
	if s.SensorLink == nil || s.Evidence == nil {
		return errors.New("not a SensorLink observation")
	}
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != s.Evidence.SHA256 || int64(len(data)) != s.Evidence.Size {
		return errors.New("JPEG bytes differ from observation")
	}
	if now == nil {
		now = time.Now
	}
	started := now()
	fresh := func() bool {
		at := now()
		return !at.Before(started) && !at.Before(component.Timing.SampleTime) && at.Before(component.Timing.SampleTime.Add(component.TTL-component.Timing.ErrorBound)) && at.Before(component.Timing.SampleTime.Add(time.Duration(s.Camera.CalibrationValidForNS)-component.Timing.ErrorBound))
	}
	if !fresh() {
		return errors.New("capture expired before media upload")
	}
	out, uploadErr := p.UploadEvidence(ctx, data, s.Evidence.ContentType, time.Time{})
	var retain int64
	if uploadErr == nil {
		if out.SHA256 != s.Evidence.SHA256 || out.Size != s.Evidence.Size || out.ContentType != s.Evidence.ContentType || !out.Complete || out.Received != out.Size {
			return errors.New("invalid SensorLink media acknowledgement")
		}
		retain = out.RetainUntilUnixMS
	} else {
		got, meta, readErr := p.DownloadEvidence(ctx, s.Evidence.SHA256, s.Evidence.Size)
		if readErr != nil {
			return uploadErr
		}
		if meta.SHA256 != s.Evidence.SHA256 || meta.Size != s.Evidence.Size || meta.ContentType != s.Evidence.ContentType || !bytes.Equal(got, data) {
			return errors.New("existing SensorLink media differs")
		}
		retain = meta.RetainUntilUnixMS
	}
	component.TTL = min(component.TTL, time.UnixMilli(retain).Sub(component.Timing.SampleTime))
	if component.TTL <= component.Timing.ErrorBound || !fresh() {
		return errors.New("capture or media expired during upload")
	}
	return p.Publish(ctx, component)
}
