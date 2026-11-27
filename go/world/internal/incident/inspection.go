package incident

import (
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"time"
)

// InspectionSelection prepares review context only from a frozen, current,
// consistent evaluation. It never signs an operator instruction or moves a robot.
func InspectionSelection(cfg Config, reader string, version world.Version, result Result, now time.Time) (world.Component, error) {
	c, err := Component(cfg, reader, version, result)
	if err != nil {
		return c, err
	}
	if result.State != "consistent" || !now.Before(result.ValidUntil) || now.Before(result.At) || result.Visual.Claim == nil || result.Gas.State != "localized" || len(result.Supports) != 2 {
		return world.Component{}, errors.New("current consistent localized incident required")
	}
	p := missioncontrol.IncidentSelection{Schema: missioncontrol.IncidentInspectionType, IncidentSHA256: gasreplica.ComponentHash(c), Contract: result.Contract, InputDigest: result.InputDigest, State: result.State, Position: result.Visual.Claim.Result.Spatial.PositionM, Uncertainty: result.Visual.Claim.Result.Spatial.UncertaintyM, ValidUntil: result.ValidUntil}
	for _, s := range result.Supports {
		p.Supports = append(p.Supports, missioncontrol.IncidentSupport{Kind: s.Kind, SHA256: s.Hash, Source: s.Source, Version: s.Version, Captured: s.Captured, ValidUntil: s.ValidUntil})
	}
	c.Key.Type = missioncontrol.IncidentInspectionType
	c.Key.Entity = cfg.Visual.Simulation.RunID + "/incident-inspection"
	c.Provenance.AppVersion = "world-incident-inspection-v1"
	c.Provenance.ModelID = "incident-inspection-selection"
	c.Provenance.ModelVersion = "1"
	c.Payload, err = json.Marshal(p)
	if err != nil {
		return world.Component{}, err
	}
	_, err = missioncontrol.IncidentTriggerTarget(c)
	return c, err
}
