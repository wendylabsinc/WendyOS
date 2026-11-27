// Package stationarycamera retains original stationary-camera JPEGs and their
// receipt observations. Receipt freshness never establishes exposure freshness.
package stationarycamera

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	worldstore "github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const (
	FreshFor      = 3 * time.Second
	Retention     = time.Hour
	MaxJPEGBytes  = 8 << 20
	MaxRecent     = 64
	maxSessions   = 128
	componentType = "wendy.sensor.camera-receipt.v1"
)

var (
	ErrFrame    = errors.New("invalid stationary camera JPEG")
	ErrSequence = errors.New("camera session or sequence is repeated, retired, or out of order")
	ErrClosed   = errors.New("stationary camera store closed")
)

type Config struct {
	Dir, Tenant, Site, Device, Camera string
	// SimulationRun binds an emulated stream to one run for the lifetime of this directory.
	SimulationRun string
	Now           func() time.Time
}

type Camera struct {
	Device string `json:"device"`
	ID     string `json:"id"`
	Label  string `json:"label"`
}

type Receipt struct {
	ID                string       `json:"id"`
	SHA256            string       `json:"sha256"`
	Width             int          `json:"width"`
	Height            int          `json:"height"`
	Size              int64        `json:"size"`
	ContentType       string       `json:"content_type"`
	Representation    string       `json:"representation"`
	SourceCodec       string       `json:"source_codec"`
	ReceivedAt        time.Time    `json:"received_at"`
	RetainedUntil     time.Time    `json:"retained_until"`
	SourceSession     string       `json:"source_session"`
	Sequence          uint64       `json:"sequence"`
	Origin            world.Origin `json:"origin"`
	SimulationRun     string       `json:"simulation_run,omitempty"`
	ExposureTimeKnown bool         `json:"exposure_time_known"`
	CalibrationStatus string       `json:"calibration_status"`
}

type State struct {
	Schema            string    `json:"schema"`
	Camera            Camera    `json:"camera"`
	Connection        string    `json:"connection"`
	Detail            string    `json:"detail"`
	Fresh             bool      `json:"fresh"`
	Freshness         string    `json:"freshness"`
	ExposureTimeKnown bool      `json:"exposure_time_known"`
	CalibrationStatus string    `json:"calibration_status"`
	Latest            *Receipt  `json:"latest"`
	Recent            []Receipt `json:"recent"`
}

type saved struct {
	Version  int               `json:"version"`
	Origin   world.Origin      `json:"origin"`
	Recent   []Receipt         `json:"recent"`
	Sessions map[string]uint64 `json:"sessions"`
}

type Store struct {
	mu                 sync.Mutex
	cfg                Config
	world              *worldstore.Store
	evidence           *evidence.Store
	source             world.Source
	key                world.Key
	frame              world.FrameRef
	clockDomain        string
	epoch, sequence    uint64
	saved              saved
	retired            map[string]bool
	currentSession     string
	connection, detail string
	received, deadline time.Time
	closed             bool
}

func validName(s string) bool { return s != "" && len(s) <= 256 && !strings.ContainsAny(s, "\x00\r\n") }

func Open(cfg Config) (*Store, error) {
	if cfg.Dir == "" || !validName(cfg.Tenant) || !validName(cfg.Site) || !validName(cfg.Device) || !validName(cfg.Camera) {
		return nil, errors.New("stationary camera requires directory, tenant, site, device and camera")
	}
	if cfg.SimulationRun != "" && (!validName(cfg.SimulationRun) || strings.TrimSpace(cfg.SimulationRun) == "") {
		return nil, errors.New("invalid camera simulation run ID")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	identity, _ := json.Marshal([]string{cfg.Tenant, cfg.Site, cfg.Device, cfg.Camera})
	digest := sha256.Sum256(identity)
	id := hex.EncodeToString(digest[:])
	s := &Store{cfg: cfg, retired: map[string]bool{}, connection: "connecting", saved: saved{Version: 1, Recent: []Receipt{}, Sessions: map[string]uint64{}}}
	s.source = world.Source{Device: cfg.Device, App: "wendy-world-stationary-camera", Service: "receipt", Stream: cfg.Camera}
	s.key = world.Key{Tenant: cfg.Tenant, Site: cfg.Site, Entity: "camera:" + id, Type: componentType, Source: s.source}
	// This camera-local frame has no asserted transform into a room or map.
	s.frame = world.FrameRef{ID: "camera:" + id, Revision: "unlocated-v1"}
	s.clockDomain = "local-receipt:" + id
	var err error
	s.world, err = worldstore.Open(filepath.Join(cfg.Dir, "world.json"), world.Config{Tenant: cfg.Tenant, Site: cfg.Site, ClockDomain: s.clockDomain, Now: cfg.Now, MaxEntries: 16, AllowSynthetic: true, Frames: []world.FrameRef{s.frame}})
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	s.evidence, err = evidence.Open(filepath.Join(cfg.Dir, "evidence"), evidence.Config{Tenant: cfg.Tenant, Site: cfg.Site, QuotaBytes: 128 << 20, MaxBlobBytes: MaxJPEGBytes, MaxEntries: 512, MaxRetention: Retention, Now: cfg.Now})
	if err != nil {
		return nil, err
	}
	records, err := s.world.Records()
	if err != nil {
		return nil, err
	}
	for _, c := range records {
		if c.Key != s.key || c.Tombstone {
			return nil, errors.New("unexpected stationary camera record")
		}
		d := json.NewDecoder(bytes.NewReader(c.Payload))
		d.DisallowUnknownFields()
		if d.Decode(&s.saved) != nil || s.saved.Version != 1 || len(s.saved.Recent) == 0 || len(s.saved.Recent) > MaxRecent || len(s.saved.Sessions) == 0 || len(s.saved.Sessions) > maxSessions || s.saved.Origin != world.Measured && s.saved.Origin != world.Simulated {
			return nil, errors.New("invalid stationary camera history")
		}
		if c.Provenance.Origin != s.saved.Origin {
			return nil, errors.New("camera history origin does not match provenance")
		}
		if s.saved.Origin == world.Simulated {
			if c.Provenance.Simulation == nil || c.Provenance.Simulation.RunID != s.simulationRun() {
				return nil, errors.New("camera simulation run cannot change in a retained directory")
			}
		} else if cfg.SimulationRun != "" {
			return nil, errors.New("physical camera history cannot be reopened as simulation")
		}
		for _, receipt := range s.saved.Recent {
			if receipt.Origin != s.saved.Origin || (receipt.SimulationRun != "" && (receipt.Origin != world.Simulated || receipt.SimulationRun != s.simulationRun())) {
				return nil, errors.New("camera receipt provenance differs from its retained run")
			}
		}
	}
	// Every previous stream is retired at startup. Durable evidence is history,
	// never proof that an old capture process is still connected.
	for session := range s.saved.Sessions {
		s.retired[session] = true
	}
	s.epoch, err = s.world.NextEpoch(s.source)
	if err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

func (s *Store) simulationRun() string {
	if s.cfg.SimulationRun != "" {
		return s.cfg.SimulationRun
	}
	return "camera-fixture"
}

// AcceptJPEG accepts an original JPEG from a physical MJPEG stream, or an
// explicitly simulated stream. Transcoded H264/VP8 images need a separate
// representation contract; callers must not label those as original JPEGs.
// There is deliberately no parameter for an invented exposure time or pose.
func (s *Store) AcceptJPEG(data []byte, sourceSession string, sequence uint64, origin world.Origin) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.connection == "stopped" {
		return errors.New("camera is stopped")
	}
	if !validName(sourceSession) || sequence == 0 || s.retired[sourceSession] || sequence <= s.saved.Sessions[sourceSession] {
		return ErrSequence
	}
	if _, exists := s.saved.Sessions[sourceSession]; !exists && len(s.saved.Sessions) >= maxSessions {
		return errors.New("camera session history full; select a new run directory")
	}
	if s.cfg.SimulationRun != "" && origin != world.Simulated {
		return errors.New("a configured simulation run only accepts simulated receipts")
	}
	if origin != world.Measured && origin != world.Simulated || s.saved.Origin != "" && s.saved.Origin != origin {
		return errors.New("camera origin must be explicit and cannot change in a retained run")
	}
	if len(data) < 4 || len(data) > MaxJPEGBytes || data[0] != 0xff || data[1] != 0xd8 || data[len(data)-2] != 0xff || data[len(data)-1] != 0xd9 {
		return ErrFrame
	}
	c, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || c.Width <= 0 || c.Height <= 0 || c.Width > 8192 || c.Height > 8192 || int64(c.Width)*int64(c.Height) > 32<<20 {
		return ErrFrame
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		return ErrFrame
	}
	now := s.cfg.Now()
	if now.IsZero() {
		return errors.New("camera receipt clock unavailable")
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	var put evidence.PutResponse
	for offset := 0; offset < len(data); offset += evidence.MaxChunk {
		put, err = s.evidence.Put(s.source.Stream, evidence.PutRequest{SHA256: digest, Size: int64(len(data)), Offset: int64(offset), Data: data[offset:min(offset+evidence.MaxChunk, len(data))], ContentType: "image/jpeg", RetentionClass: evidence.RetentionRolling})
		if err != nil {
			return err
		}
	}
	if !put.Complete {
		return errors.New("camera evidence incomplete")
	}
	r := Receipt{ID: fmt.Sprintf("%d-%d", s.epoch, s.sequence+1), SHA256: digest, Width: c.Width, Height: c.Height, Size: int64(len(data)), ContentType: "image/jpeg", Representation: "original_jpeg", SourceCodec: "mjpeg", ReceivedAt: now.Round(0).UTC(), RetainedUntil: time.UnixMilli(put.RetainUntilUnixMS).UTC(), SourceSession: sourceSession, Sequence: sequence, Origin: origin, CalibrationStatus: "unknown"}
	if !r.RetainedUntil.After(now) {
		return evidence.ErrUnavailable
	}
	if origin == world.Simulated {
		r.SimulationRun = s.simulationRun()
	}
	next := saved{Version: 1, Origin: origin, Recent: append([]Receipt{r}, s.saved.Recent...), Sessions: make(map[string]uint64, len(s.saved.Sessions)+1)}
	if len(next.Recent) > MaxRecent {
		next.Recent = next.Recent[:MaxRecent]
	}
	for session, seq := range s.saved.Sessions {
		next.Sessions[session] = seq
	}
	next.Sessions[sourceSession] = sequence
	payload, err := json.Marshal(next)
	if err != nil {
		return err
	}
	provenance := world.Provenance{Origin: origin, AppVersion: "world-stationary-camera-v1"}
	if origin == world.Simulated {
		provenance.Simulation = &world.SimulationContext{RunID: s.simulationRun(), ScenarioVersion: "stationary-camera-v1", ClockDomain: s.clockDomain}
	}
	component := world.Component{Key: s.key, Version: world.Version{Epoch: s.epoch, Sequence: s.sequence + 1}, Frame: s.frame, Timing: world.Timing{SampleTime: now, ClockDomain: s.clockDomain, ErrorKnown: true}, TTL: FreshFor, Provenance: provenance, Payload: payload}
	if _, err := s.world.Apply(s.source, component); err != nil {
		return err
	}
	if s.currentSession != "" && s.currentSession != sourceSession {
		s.retired[s.currentSession] = true
	}
	s.saved = next
	s.sequence++
	s.currentSession = sourceSession
	s.received = now
	s.deadline = now.Add(FreshFor)
	s.connection = "streaming"
	s.detail = ""
	return nil
}

// SetConnection invalidates freshness immediately. Reconnecting must supply a
// new source session, so buffered frames from a failed stream cannot revive it.
func (s *Store) SetConnection(status, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	switch status {
	case "connecting", "disconnected", "error", "stopped":
	default:
		status = "error"
		detail = "invalid camera connection state"
	}
	if s.currentSession != "" {
		s.retired[s.currentSession] = true
	}
	s.currentSession = ""
	s.deadline = time.Time{}
	s.received = time.Time{}
	if len(detail) > 512 {
		detail = detail[:512]
	}
	s.connection = status
	s.detail = detail
}

func (s *Store) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	state := State{Schema: "wendy.stationary-camera.state.v1", Camera: Camera{Device: s.cfg.Device, ID: s.cfg.Camera, Label: s.cfg.Camera}, Connection: s.connection, Detail: s.detail, Freshness: s.connection, CalibrationStatus: "unknown", Recent: []Receipt{}}
	for _, r := range s.saved.Recent {
		if r.RetainedUntil.After(now) {
			state.Recent = append(state.Recent, r)
		}
	}
	if len(s.saved.Recent) > 0 {
		latest := s.saved.Recent[0]
		state.Latest = &latest
	}
	state.Fresh = !s.closed && s.connection == "streaming" && !s.received.IsZero() && !now.Before(s.received) && now.Before(s.deadline) && state.Latest != nil && state.Latest.RetainedUntil.After(now)
	if state.Fresh {
		state.Freshness = "receiving"
	} else if state.Connection == "streaming" {
		state.Freshness = "stale"
	} else if state.Connection == "connecting" {
		state.Freshness = "waiting"
	}
	return state
}

func (s *Store) Evidence(digest string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	// The private viewer is authorized for this camera's retained evidence,
	// independently of the 64-row presentation window. Otherwise selecting the
	// oldest row could race with the next frame and lose a still-retained image.
	// GetOwned obtains trusted metadata and verifies both ownership and bytes;
	// no caller-provided size or MIME type can substitute for that manifest.
	meta, err := s.evidence.GetOwned(s.source.Stream, evidence.GetRequest{SHA256: digest, Offset: 0, Length: 1})
	if err != nil {
		return nil, err
	}
	_, data, err := s.evidence.ReadCompleteOwned(s.source.Stream, evidence.Reference{SHA256: meta.SHA256, Size: meta.Size, ContentType: meta.ContentType})
	return data, err
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.connection = "stopped"
	s.deadline = time.Time{}
	var errs []error
	if s.evidence != nil {
		errs = append(errs, s.evidence.Close())
	}
	if s.world != nil {
		errs = append(errs, s.world.Close())
	}
	return errors.Join(errs...)
}
