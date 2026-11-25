// Package proofreader retrieves exact commissioned audit bytes through an
// ordinary World socket. It never treats historical signatures as current
// source authority, and holds no publisher or actuator credential.
package proofreader

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"golang.org/x/sys/unix"
)

var (
	ErrConfig = errors.New("invalid commissioned historical proof request")
	ErrProof  = errors.New("historical proof differs or retention expired")
	ErrMedia  = errors.New("historical evidence differs or retention expired")
	ErrLive   = errors.New("original expired version remains live or query is unknown")
)

const (
	MaxConfigBytes   = 2 << 20
	MaxResultBytes   = 32 << 20
	MaxEnvelopeBytes = 256 << 10
)

type ExpectedProof struct {
	Reference    proofref.Reference `json:"reference"`
	EnvelopeJSON []byte             `json:"envelope_json"`
}
type Config struct {
	Version            int             `json:"version"`
	Tenant             string          `json:"tenant"`
	Site               string          `json:"site"`
	Device             string          `json:"device"`
	App                string          `json:"app"`
	Service            string          `json:"service"`
	ExpectedGeneration uint64          `json:"expected_generation"`
	RunID              string          `json:"run_id"`
	DurationMS         int64           `json:"duration_ms"`
	RequestTimeoutMS   int64           `json:"request_timeout_ms,omitempty"`
	PollIntervalMS     int64           `json:"poll_interval_ms,omitempty"`
	ClockMaxWidthMS    int64           `json:"clock_max_width_ms,omitempty"`
	Expected           []ExpectedProof `json:"expected"`
}
type expectedRecord struct {
	expected ExpectedProof
	record   serviceorigin.Record
	capture  *sensorrecord.Capture
}
type prepared struct {
	cfg          Config
	records      []expectedRecord
	expiredAfter time.Time
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func decode(raw []byte) (serviceorigin.Record, proofref.Reference, error) {
	var s serviceorigin.SignedRecord
	var r serviceorigin.Record
	var d serviceorigin.DelegationClaims
	if len(raw) == 0 || len(raw) > MaxEnvelopeBytes || authority.DecodeStrictLimit(raw, &s, MaxEnvelopeBytes) != nil || jws.DecodeStrict(s.Record, &r) != nil || jws.DecodeStrict(s.Delegation.Claims, &d) != nil || len(s.Signature) != 64 || len(s.Delegation.Signature) != 3309 || len(s.Delegation.ChainDER) < 1 || len(s.Delegation.ChainDER) > 8 || len(d.PublicKey) != 32 || r.Version != 1 || r.AuthorityDomain != "physical" || r.Scope.Validate() != nil || r.Scope.ExecutionDomain != "simulation" || r.DelegationSHA256 != digest(s.Delegation.Claims) || d.Version != 1 || d.AuthorityDomain != "physical" || d.Epoch != r.Epoch || !reflect.DeepEqual(d.Scope, r.Scope) || d.IssuedAtUnixMS <= 0 || d.IssuedAtUnixMS > r.IssuedAtUnixMS || r.ExpiresAtUnixMS <= r.IssuedAtUnixMS || r.ExpiresAtUnixMS > d.ExpiresAtUnixMS {
		return r, proofref.Reference{}, ErrConfig
	}
	for _, der := range s.Delegation.ChainDER {
		if len(der) == 0 || len(der) > 32<<10 {
			return r, proofref.Reference{}, ErrConfig
		}
	}
	p := r.Publication
	ref := proofref.Reference{Key: world.Key{Tenant: r.Scope.Tenant, Site: r.Scope.Site, Entity: p.Entity, Type: p.ComponentType, Source: world.Source{Device: r.Scope.Device, App: r.Scope.App, Service: r.Scope.Service, Stream: p.Stream}}, Version: world.Version{Epoch: r.Epoch, Sequence: r.Sequence}, EnvelopeSHA256: digest(raw), RecordSHA256: digest(s.Record)}
	if ref.Validate() != nil || p.Tombstone || p.TTL <= 0 || p.Frame.Validate() != nil || p.Timing.Validate() != nil || p.Provenance.Validate() != nil || len(r.Scope.Publish) != 1 || r.Scope.Publish[0] != (serviceorigin.PublishScope{ComponentType: p.ComponentType, Stream: p.Stream}) {
		return r, ref, ErrConfig
	}
	return r, ref, nil
}
func component(r serviceorigin.Record) world.Component {
	return (serviceorigin.VerifiedRecord{Record: r}).Component()
}
func prepare(c Config) (prepared, error) {
	if c.RequestTimeoutMS == 0 {
		c.RequestTimeoutMS = 1000
	}
	if c.PollIntervalMS == 0 {
		c.PollIntervalMS = 500
	}
	if c.ClockMaxWidthMS == 0 {
		c.ClockMaxWidthMS = 5000
	}
	scope := serviceorigin.Scope{Tenant: c.Tenant, Site: c.Site, Device: c.Device, App: c.App, Service: c.Service, DeploymentGeneration: c.ExpectedGeneration, ExecutionDomain: "simulation"}
	if c.Version != 1 || scope.Validate() != nil || c.ExpectedGeneration == 0 || c.DurationMS < 1 || c.DurationMS > 60000 || c.RequestTimeoutMS < 10 || c.RequestTimeoutMS > 1000 || c.PollIntervalMS < 250 || c.PollIntervalMS > 5000 || c.ClockMaxWidthMS < 1 || c.ClockMaxWidthMS > 10000 || len(c.Expected) != 5 {
		return prepared{}, ErrConfig
	}
	out := prepared{cfg: c}
	out.cfg.Expected = make([]ExpectedProof, 5)
	sources := map[proofref.Source]bool{}
	sensors := map[string]bool{}
	var inputs []resultrecord.ValidatedInput
	var claim *serviceorigin.Record
	var first *sensorrecord.Capture
	total := 0
	for i, e := range c.Expected {
		total += len(e.EnvelopeJSON)
		if total > 1<<20 {
			return prepared{}, ErrConfig
		}
		r, ref, err := decode(e.EnvelopeJSON)
		if err != nil || ref != e.Reference || r.Scope.Tenant != c.Tenant || r.Scope.Site != c.Site || r.Scope.Device != c.Device || r.Scope.App == c.App && r.Scope.Service == c.Service || sources[ref.Source()] || r.Publication.Timing.ClockDomain != "simulation:"+c.RunID {
			return prepared{}, ErrConfig
		}
		sources[ref.Source()] = true
		e.EnvelopeJSON = bytes.Clone(e.EnvelopeJSON)
		out.cfg.Expected[i] = e
		row := expectedRecord{expected: e, record: r}
		switch r.Publication.ComponentType {
		case sensorrecord.ComponentType:
			capture, err := sensorrecord.FromComponent(component(r))
			if err != nil {
				return prepared{}, ErrConfig
			}
			sample, err := capture.Sample()
			if err != nil || sample.RunID != c.RunID || sensors[sample.SensorID] {
				return prepared{}, ErrConfig
			}
			switch sample.SensorID {
			case "camera-a", "camera-b", "gas-1", "audio-1":
			default:
				return prepared{}, ErrConfig
			}
			sensors[sample.SensorID] = true
			if first != nil {
				f, _ := first.Sample()
				if capture.SceneBootID != first.SceneBootID || capture.SimulationAnchorUnixNS != first.SimulationAnchorUnixNS || sample.PackageSHA256 != f.PackageSHA256 || sample.Frame != f.Frame || sample.ScenarioVersion != f.ScenarioVersion || sample.Seed != f.Seed {
					return prepared{}, ErrConfig
				}
			} else {
				copy := capture
				first = &copy
			}
			sampleUntil, physicalUntil, err := sensorrecord.LiveDeadlines(component(r))
			if err != nil {
				return prepared{}, ErrConfig
			}
			if physicalUntil.After(out.expiredAfter) {
				out.expiredAfter = physicalUntil
			}
			inputs = append(inputs, resultrecord.ValidatedInput{Reference: ref, Component: component(r), SampleValidUntil: sampleUntil, PhysicalValidUntil: physicalUntil})
			row.capture = &capture
		case resultrecord.ComponentType:
			if claim != nil {
				return prepared{}, ErrConfig
			}
			copy := r
			claim = &copy
		default:
			return prepared{}, ErrConfig
		}
		out.records = append(out.records, row)
	}
	if len(sensors) != 4 || claim == nil || out.expiredAfter.IsZero() {
		return prepared{}, ErrConfig
	}
	if _, err := resultrecord.ValidatePublication(claim.Publication, inputs); err != nil {
		return prepared{}, ErrConfig
	}
	return out, nil
}
func LoadConfig(path string) (Config, error) {
	var c Config
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return c, ErrConfig
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return c, ErrConfig
	}
	f := os.NewFile(uintptr(fd), "proof-reader-config")
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || st.Size() < 1 || st.Size() > MaxConfigBytes {
		return c, ErrConfig
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil || authority.DecodeStrictLimit(raw, &c, MaxConfigBytes) != nil {
		return c, ErrConfig
	}
	checked, err := prepare(c)
	return checked.cfg, err
}
