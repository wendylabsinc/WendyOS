package readservice

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
)

var cursorPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func bounded(value string, max int) bool {
	return value != "" && len(value) <= max && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}
func validateDataset(d Dataset) error {
	if d.Domain != "simulation" || !bounded(d.ID, 128) || !bounded(d.SourceTenant, 256) || !bounded(d.SourceSite, 256) || !bounded(d.Frame.ID, 256) || !bounded(d.Frame.Revision, 256) || !bounded(d.RunID, 256) {
		return errors.New("only an explicit simulation dataset is currently supported")
	}
	return nil
}

func validSimulation(s *world.SimulationContext, d Dataset) bool {
	return s != nil && s.RunID == d.RunID && bounded(s.ScenarioVersion, 256) && s.ClockDomain == "simulation:"+d.RunID
}
func validProvenance(p world.Provenance, d Dataset) bool {
	if p.Validate() != nil || !bounded(p.AppVersion, 256) {
		return false
	}
	switch p.Origin {
	case "simulated", "authored":
		if !validSimulation(p.Simulation, d) || len(p.Inputs) != 0 || p.OriginalCaptureTime != nil || p.OriginalClockDomain != "" {
			return false
		}
		if p.Origin == "authored" && (p.ModelID != "" || p.ModelVersion != "") {
			return false
		}
		return true
	case "inferred":
		if !bounded(p.ModelID, 256) || !bounded(p.ModelVersion, 256) || p.Simulation != nil || p.OriginalCaptureTime != nil || p.OriginalClockDomain != "" || len(p.Inputs) == 0 || len(p.Inputs) > MaxComponents {
			return false
		}
		seen := map[string]bool{}
		for _, input := range p.Inputs {
			if !bounded(input.ID, 512) || seen[input.ID] || input.Origin != "simulated" || !validSimulation(input.Simulation, d) || input.OriginalCaptureTime != nil || input.OriginalClockDomain != "" {
				return false
			}
			seen[input.ID] = true
		}
		return true
	default:
		// Replayed/mixed/physical sources need a separately reviewed adapter.
		return false
	}
}

func validateSnapshot(snapshot Snapshot, dataset Dataset, limit int) error {
	if snapshot.Dataset != dataset || validateDataset(snapshot.Dataset) != nil || !cursorPattern.MatchString(snapshot.Cursor) || snapshot.Components == nil || len(snapshot.Components) > limit {
		return errors.New("snapshot metadata or size mismatch")
	}
	keys := map[string]bool{}
	for _, raw := range snapshot.Components {
		if len(raw) == 0 || len(raw) > MaxComponentBytes {
			return errors.New("component exceeds bound")
		}
		var c world.Component
		if err := jws.DecodeStrict(raw, &c); err != nil {
			return err
		}
		if c.Key.Tenant != dataset.SourceTenant || c.Key.Site != dataset.SourceSite || !bounded(c.Key.Entity, 256) || !bounded(c.Key.Type, 256) || !bounded(c.Key.Source.Device, 512) || !bounded(c.Key.Source.App, 256) || !bounded(c.Key.Source.Service, 256) || !bounded(c.Key.Source.Stream, 256) || (c.Frame.ID != dataset.Frame.ID || c.Frame.Revision != dataset.Frame.Revision) || c.Version.Epoch == 0 || c.Version.Sequence == 0 || c.Tombstone || c.TTL <= 0 || c.TTL > 30*24*time.Hour || c.Timing.SampleTime.IsZero() || c.Timing.ClockDomain != "simulation:"+dataset.RunID || !c.Timing.ErrorKnown || c.Timing.ErrorBound < 0 || c.Timing.AgeHint < 0 || len(c.Payload) == 0 || !validProvenance(c.Provenance, dataset) {
			return errors.New("component scope or simulation provenance mismatch")
		}
		key, _ := json.Marshal(c.Key)
		if keys[string(key)] {
			return errors.New("duplicate component key")
		}
		keys[string(key)] = true
	}
	return nil
}
