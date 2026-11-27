package fusiondemo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Evaluator is used by one participant process, with serial ticks. A reopened
// evaluator observes the current simulation time before opening its stores.
type Evaluator struct {
	cfg                       Config
	dir                       string
	catalog                   map[string]Sensor
	now                       time.Time
	replica                   *store.Store
	evidence                  *evidence.Store
	closed                    bool
	claimEpoch, claimSequence uint64
}

func New(cfg Config, dir string) (*Evaluator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if dir == "" {
		return nil, errors.New("participant state directory required")
	}
	cfg.Sensors = append([]Sensor(nil), cfg.Sensors...)
	cfg.CameraQuorum = append([]string(nil), cfg.CameraQuorum...)
	e := &Evaluator{cfg: cfg, dir: dir, catalog: map[string]Sensor{}}
	for _, s := range cfg.Sensors {
		e.catalog[s.SensorID] = s
	}
	return e, nil
}

func (e *Evaluator) open() error {
	if e.replica != nil {
		return nil
	}
	if err := os.MkdirAll(e.dir, 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(e.cfg)
	if err != nil {
		return err
	}
	path := filepath.Join(e.dir, "participant.json")
	if old, err := os.ReadFile(path); err == nil {
		if !bytes.Equal(old, raw) {
			return errors.New("state belongs to a different participant catalog")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(raw)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	} else {
		return err
	}
	r, err := store.Open(filepath.Join(e.dir, "world.json"), world.Config{Tenant: Tenant, Site: Site, ClockDomain: "simulation:" + e.cfg.RunID, Now: func() time.Time { return e.now }, MaxEntries: len(e.catalog), AllowSynthetic: true, Frames: []world.FrameRef{e.cfg.Frame}})
	if err != nil {
		return err
	}
	media, err := evidence.Open(filepath.Join(e.dir, "media"), evidence.Config{Tenant: Tenant, Site: Site, Now: func() time.Time { return e.now }, QuotaBytes: 128 << 20, MaxBlobBytes: MaxMediaBytes, MaxEntries: 2048, MaxRetention: 7 * 24 * time.Hour})
	if err != nil {
		r.Close()
		return err
	}
	epoch, err := r.NextEpoch(e.claimSource())
	if err != nil {
		r.Close()
		media.Close()
		return err
	}
	e.claimEpoch = epoch
	e.replica, e.evidence = r, media
	return nil
}

func (e *Evaluator) converter(sensor Sensor) (*sensing.Converter, error) {
	return sensing.New(sensing.Config{Tenant: Tenant, Site: Site, RunID: e.cfg.RunID, Frame: e.cfg.Frame, Source: sensor.Source(), Epoch: 1, Anchor: Anchor, TTL: TTL})
}

// validateComponent checks the original simulation conversion. A received
// component is applied unchanged; only its transit age hint may have increased.
func (e *Evaluator) validateComponent(c world.Component) (sensing.Sample, error) {
	s, err := sensing.FromComponent(c)
	if err != nil {
		return s, err
	}
	entry, ok := e.catalog[s.SensorID]
	if !ok || entry.Kind != s.Kind || c.Key.Source != entry.Source() || c.TTL != TTL || c.Version.Epoch != 1 {
		return s, errors.New("received observation differs from catalog")
	}
	converter, err := e.converter(entry)
	if err != nil {
		return s, err
	}
	expected, err := converter.Convert(c.Payload)
	if err != nil {
		return s, err
	}
	copy := c
	copy.Timing.AgeHint = 0
	if !reflect.DeepEqual(expected, copy) {
		return s, errors.New("received original component differs")
	}
	return s, nil
}

func (e *Evaluator) Tick(ctx context.Context, tick Tick) (Output, error) {
	out := Output{ParticipantID: e.cfg.ParticipantID, NowNS: tick.NowNS, Observations: []sensing.Sample{}, ObservationStatus: []ObservationStatus{}, MissingObservations: []Missing{}, ReplicaRecords: []world.Component{}, Media: map[string][]byte{}}
	if e.closed || tick.NowNS < 0 || tick.NowNS > int64(24*time.Hour) || len(tick.LocalSamples) > len(e.catalog) || len(tick.Received) > len(e.catalog) {
		return out, errors.New("invalid participant tick")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	now := Anchor.Add(time.Duration(tick.NowNS))
	if !e.now.IsZero() && now.Before(e.now) {
		return out, errors.New("participant clock regressed")
	}
	e.now = now
	if err := e.open(); err != nil {
		return out, err
	}
	retained, err := e.replica.Records()
	if err != nil {
		return out, err
	}
	bySensor := map[string]world.Component{}
	refs := map[string]sensing.EvidenceRef{}
	for _, c := range retained {
		s, err := e.validateComponent(c)
		if err != nil {
			return out, err
		}
		bySensor[s.SensorID] = c
		if s.Evidence != nil {
			refs[s.Evidence.SHA256] = *s.Evidence
		}
	}
	// Validate and convert the whole incoming batch before media or register
	// writes. Each individual store mutation remains the existing durable API.
	updates := make([]world.Component, 0, len(tick.LocalSamples)+len(tick.Received))
	seen := map[string]bool{}
	for _, raw := range tick.LocalSamples {
		s, err := sensing.Decode(raw)
		if err != nil {
			return out, err
		}
		entry, ok := e.catalog[s.SensorID]
		if !ok || entry.Owner != e.cfg.ParticipantID || entry.Kind != s.Kind || seen[s.SensorID] {
			return out, errors.New("local sample is not owned by participant")
		}
		seen[s.SensorID] = true
		converter, err := e.converter(entry)
		if err != nil {
			return out, err
		}
		if old, ok := bySensor[s.SensorID]; ok && s.Sequence > old.Version.Sequence {
			if _, err = converter.Convert(old.Payload); err != nil {
				return out, err
			}
		}
		component, err := converter.Convert(raw)
		if err != nil {
			return out, err
		}
		updates = append(updates, component)
	}
	for _, c := range tick.Received {
		if _, err := e.validateComponent(c); err != nil {
			return out, err
		}
		updates = append(updates, c)
	}
	for _, c := range updates {
		s, err := e.validateComponent(c)
		if err != nil {
			return out, err
		}
		if s.Evidence != nil {
			ref := *s.Evidence
			if old, ok := refs[ref.SHA256]; ok && old != ref {
				return out, errors.New("conflicting media metadata")
			}
			refs[ref.SHA256] = ref
		}
	}
	total := 0
	for hash, data := range tick.Media {
		total += len(data)
		ref, ok := refs[hash]
		sum := sha256.Sum256(data)
		if !ok || int64(len(data)) != ref.Size || hex.EncodeToString(sum[:]) != hash || total > MaxMediaBytes {
			return out, errors.New("invalid delivered media")
		}
	}
	for hash, data := range tick.Media {
		ref := refs[hash]
		if err := e.put(ref, data); err != nil {
			return out, err
		}
	}
	for _, c := range updates {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		result, err := e.replica.Apply(c.Key.Source, c)
		if err != nil {
			return out, err
		}
		switch result {
		case world.Applied:
			out.Applied++
		case world.Duplicate:
			out.Duplicates++
		case world.Obsolete:
			out.Obsolete++
		}
	}
	live, err := e.replica.Snapshot()
	if err != nil {
		return out, err
	}
	out.ReplicaRecords, err = e.replica.Records()
	if err != nil {
		return out, err
	}
	held := map[string]bool{}
	known := map[string]bool{}
	mediaBytes := 0
	for _, c := range out.ReplicaRecords {
		known[c.Key.Source.Service] = true
	}
	for _, c := range live {
		s, err := sensing.FromComponent(c)
		if err != nil {
			return out, err
		}
		out.Observations = append(out.Observations, s)
		held[s.SensorID] = true
		owner := e.catalog[s.SensorID].Owner
		delivery := "received"
		if owner == e.cfg.ParticipantID {
			delivery = "local"
		}
		out.ObservationStatus = append(out.ObservationStatus, ObservationStatus{ID: s.SensorID, SourceNode: owner, Delivery: delivery, AgeS: max(0, e.now.Sub(c.Timing.SampleTime).Seconds())})
		if s.Evidence != nil {
			data, err := e.load(ctx, *s.Evidence)
			if err == nil {
				if _, exists := out.Media[s.Evidence.SHA256]; !exists {
					mediaBytes += len(data)
				}
				if mediaBytes > MaxMediaBytes {
					return out, errors.New("live media exceeds participant export bound")
				}
				out.Media[s.Evidence.SHA256] = data
			} else if !errors.Is(err, evidence.ErrUnavailable) {
				return out, err
			}
		}
	}
	for _, s := range e.cfg.Sensors {
		if !held[s.SensorID] {
			reason := "not_received"
			if known[s.SensorID] {
				reason = "expired"
			}
			out.MissingObservations = append(out.MissingObservations, Missing{ID: s.SensorID, Kind: s.Kind, SourceNode: s.Owner, Reason: reason})
		}
	}
	cfg := fusion.Config{CameraQuorum: e.cfg.CameraQuorum, Tenant: Tenant, Site: Site, RunID: e.cfg.RunID, ClockDomain: "simulation:" + e.cfg.RunID, Frame: e.cfg.Frame, Now: func() time.Time { return e.now }, RequiredSensors: map[string]string{}, OptionalSensors: map[string]string{}, Sources: map[string]world.Source{}}
	for _, s := range e.cfg.Sensors {
		cfg.Sources[s.SensorID] = s.Source()
		if s.Kind == "camera" && len(e.cfg.CameraQuorum) > 0 {
			continue
		}
		if s.Kind == "lidar" {
			cfg.OptionalSensors[s.SensorID] = s.Kind
		} else {
			cfg.RequiredSensors[s.SensorID] = s.Kind
		}
	}
	out.Result, err = fusion.Evaluate(ctx, cfg, live, func(_ context.Context, ref sensing.EvidenceRef) ([]byte, error) {
		data, ok := out.Media[ref.SHA256]
		if !ok {
			return nil, evidence.ErrUnavailable
		}
		return bytes.Clone(data), nil
	})
	if err != nil {
		return out, err
	}
	e.claimSequence++
	out.Version = world.Version{Epoch: e.claimEpoch, Sequence: e.claimSequence}
	if out.Result.State == fusion.HazardDetected {
		claim, err := out.Result.Component(e.claimSource(), e.claimEpoch, e.claimSequence)
		if err != nil {
			return out, err
		}
		out.Claim = &claim
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	if len(encoded) > MaxLineBytes {
		return out, errors.New("participant response exceeds bound")
	}
	return out, ctx.Err()
}

func (e *Evaluator) put(ref sensing.EvidenceRef, data []byte) error {
	if _, _, err := e.evidence.ReadCompleteOwned(e.cfg.ParticipantID, evidence.Reference{SHA256: ref.SHA256, Size: ref.Size, ContentType: ref.ContentType}); err == nil {
		return nil
	} else if !errors.Is(err, evidence.ErrUnavailable) {
		return err
	}
	for offset := 0; offset < len(data); {
		end := min(offset+evidence.MaxChunk, len(data))
		_, err := e.evidence.Put(e.cfg.ParticipantID, evidence.PutRequest{SHA256: ref.SHA256, Size: ref.Size, Offset: int64(offset), Data: data[offset:end], ContentType: ref.ContentType, RetentionClass: evidence.RetentionRolling, RetainUntilUnixMS: Anchor.Add(24*time.Hour + TTL).UnixMilli()})
		if err != nil {
			return err
		}
		offset = end
	}
	return nil
}
func (e *Evaluator) load(_ context.Context, ref sensing.EvidenceRef) ([]byte, error) {
	_, data, err := e.evidence.ReadCompleteOwned(e.cfg.ParticipantID, evidence.Reference{SHA256: ref.SHA256, Size: ref.Size, ContentType: ref.ContentType})
	return data, err
}
func (e *Evaluator) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	var a, b error
	if e.replica != nil {
		a = e.replica.Close()
	}
	if e.evidence != nil {
		b = e.evidence.Close()
	}
	if a != nil || b != nil {
		return fmt.Errorf("participant close: %w", errors.Join(a, b))
	}
	return nil
}

func (e *Evaluator) claimSource() world.Source {
	return world.Source{Device: e.cfg.ParticipantID, App: "world-fusion-demo", Service: "hazard", Stream: "claims"}
}
