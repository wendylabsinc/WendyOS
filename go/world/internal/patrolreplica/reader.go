package patrolreplica

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type Progress struct {
	RunID           string        `json:"run_id"`
	SpecSHA256      string        `json:"spec_sha256"`
	OriginalSource  world.Source  `json:"original_source"`
	OriginalVersion world.Version `json:"original_version"`
	State           patrol.State  `json:"state"`
	ObservedMS      int64         `json:"observed_unix_ms"`
	ExpiresMS       int64         `json:"expires_unix_ms"`
}
type View struct {
	Schema    string    `json:"schema"`
	Config    Config    `json:"config"`
	Reader    string    `json:"reader"`
	UpdatedMS int64     `json:"updated_unix_ms"`
	Connected bool      `json:"connected"`
	Live      *Progress `json:"live,omitempty"`
	Reports   []Report  `json:"reports"`
	Rejected  int       `json:"rejected"`
	Error     string    `json:"error,omitempty"`
}

// Select admits only this publisher's configured report contract. A retained
// report is historical even while its transport/retention TTL remains valid.
func Select(c Config, components []world.Component, now time.Time) (*Progress, []Report, int) {
	var live *Progress
	reports := []Report{}
	bad := 0
	seen := map[string]string{}
	for _, v := range components {
		if v.Key.Type != LiveType && v.Key.Type != ReportType {
			continue
		}
		if v.Key.Tenant != c.Tenant || v.Key.Site != c.Site || v.Key.Source != c.Source || v.Frame != c.Frame || v.Tombstone || v.Version.Epoch == 0 || v.Version.Sequence == 0 || v.Timing.Validate() != nil || !v.Timing.ErrorKnown || v.Timing.ErrorBound != 0 || v.Timing.ClockDomain != c.Clock() || v.Timing.SampleTime.After(now) || !now.Before(v.Timing.SampleTime.Add(v.TTL)) || v.Provenance.Validate() != nil || v.Provenance.Simulation == nil || v.Provenance.Simulation.RunID != c.RunID || v.Provenance.AppVersion != "world-patrol-replica-v1" {
			bad++
			continue
		}
		if v.Key.Type == LiveType {
			var p Progress
			if json.Unmarshal(v.Payload, &p) != nil || p.RunID != c.RunID || p.SpecSHA256 != c.SpecSHA256 || p.OriginalSource != ExecutorSource || p.OriginalVersion.Epoch != 1 || p.OriginalVersion.Sequence == 0 || p.State.Schema != patrol.StatusType || p.State.MissionID != c.MissionID || v.Key.Entity != c.MissionID || v.TTL != 5*time.Second || v.Provenance.Origin != world.Simulated {
				bad++
				continue
			}
			p.ObservedMS = v.Timing.SampleTime.UnixMilli()
			p.ExpiresMS = v.Timing.SampleTime.Add(v.TTL).UnixMilli()
			if live == nil || p.OriginalVersion.Sequence > live.OriginalVersion.Sequence {
				live = &p
			}
			continue
		}
		var r Report
		if Decode(v.Payload, &r, 1<<20) != nil || r.Validate(c) != nil || v.Key.Entity != "patrol-report/"+r.ID || v.Timing.SampleTime.Add(v.TTL).UnixMilli() != c.RetainUntilMS || v.Provenance.Origin != world.Replayed || v.Provenance.OriginalCaptureTime == nil || v.Provenance.OriginalCaptureTime.UnixMilli() != r.OriginalUnixMS || v.Provenance.OriginalClockDomain != r.OriginalClock || r.OriginalUnixMS > v.Timing.SampleTime.UnixMilli() {
			bad++
			continue
		}
		if previous, ok := seen[r.ID]; ok {
			if previous != r.ContentSHA256 {
				bad++
			}
			continue
		}
		seen[r.ID] = r.ContentSHA256
		reports = append(reports, r)
	}
	sort.Slice(reports, func(i, j int) bool {
		if reports[i].OriginalUnixMS == reports[j].OriginalUnixMS {
			return reports[i].ID < reports[j].ID
		}
		return reports[i].OriginalUnixMS > reports[j].OriginalUnixMS
	})
	if len(reports) > MaxReports {
		reports = reports[:MaxReports]
		bad++
	}
	return live, reports, bad
}
func Fetch(ctx context.Context, c Config, node *client.Client, r Report, digest string) ([]byte, Evidence, error) {
	if r.Validate(c) != nil || time.Now().UnixMilli() >= r.RetainUntilMS {
		return nil, Evidence{}, errors.New("report unavailable or expired")
	}
	var ref Evidence
	for _, e := range r.Evidence {
		if e.SHA256 == digest {
			ref = e
			break
		}
	}
	if ref.SHA256 == "" {
		return nil, ref, errors.New("digest is not referenced by this report")
	}
	b, meta, e := node.DownloadEvidence(ctx, digest, ref.Size)
	if e != nil {
		return nil, ref, e
	}
	if meta.ContentType != ref.ContentType || meta.Size != ref.Size || meta.RetainUntilUnixMS != c.RetainUntilMS || time.Now().UnixMilli() >= r.RetainUntilMS {
		return nil, ref, errors.New("original metadata disagrees with report")
	}
	return b, ref, nil
}
