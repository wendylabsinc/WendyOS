package incidentchange

import (
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/gaschange"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/incident"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"time"
)

// InspectionSelection owns the exact joined publication, the complete original
// temporal claim and an existing spatial inspection context. It prepares review
// evidence only; the existing operator signature remains the motion authority.
func InspectionSelection(cfg Config, reader string, version world.Version, result Result, records []world.Component, now time.Time, verify PairVerifier) (world.Component, error) {
	joined, err := Component(cfg, reader, version, result)
	if err != nil {
		return world.Component{}, err
	}
	if result.State != "supported_rise" || now.Before(result.At) || !now.Before(result.ValidUntil) {
		return world.Component{}, errors.New("current unchanged supported rise required")
	}
	spatial, err := incident.InspectionSelection(cfg.Spatial, reader, version, result.Spatial, now)
	if err != nil {
		return world.Component{}, err
	}
	var temporal world.Component
	found := false
	candidates := own(records)
	for _, c := range records {
		if c.Key.Type == PairType {
			pair, e := ValidatePair(cfg, c, now, verify)
			if e != nil {
				continue
			}
			members, e := pair.Components()
			if e != nil {
				continue
			}
			candidates = append(candidates, members...)
		}
	}
	for _, c := range candidates {
		if c.Key.Type == gaschange.ClaimType && gasreplica.ComponentHash(c) == result.Temporal.Hash {
			if _, e := gaschange.ValidateClaim(cfg.Temporal, c, now); e != nil {
				return world.Component{}, e
			}
			c.Timing.AgeHint = 0
			temporal = own(c)
			found = true
			break
		}
	}
	if !found {
		return world.Component{}, errors.New("exact current temporal original claim unavailable")
	}
	jb, _ := json.Marshal(joined)
	tb, _ := json.Marshal(temporal)
	sb, _ := json.Marshal(spatial)
	p := missioncontrol.RisingSelection{Schema: missioncontrol.RisingInspectionType, JoinedSHA256: gasreplica.ComponentHash(joined), JoinedJSON: jb, TemporalSHA256: gasreplica.ComponentHash(temporal), TemporalJSON: tb, SpatialJSON: sb, ValidUntil: result.ValidUntil}
	c := own(joined)
	c.Key.Type = missioncontrol.RisingInspectionType
	c.Key.Entity = cfg.Spatial.Visual.Simulation.RunID + "/rising-inspection"
	c.Provenance.AppVersion = "world-rising-inspection-v1"
	c.Provenance.ModelID = "retained-exact-rise-selection"
	c.Provenance.ModelVersion = "1"
	c.Payload, err = json.Marshal(p)
	if err != nil {
		return world.Component{}, err
	}
	if len(c.Payload) > missioncontrol.MaxRisingPayload {
		return world.Component{}, errors.New("retained rising context exceeds bounded payload")
	}
	_, err = missioncontrol.RisingTriggerTarget(c)
	return c, err
}
