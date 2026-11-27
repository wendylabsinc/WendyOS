package tasking

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
)

// ValidateControlSpec binds the immutable policy exactly. It does not consult a
// mutable proposal: after admission that reference is historical input only.
func ValidateControlSpec(s Spec, expected *missioncontrol.SignedContext, runID string) error {
	if s.InspectionDestination != nil && (s.ControlContext == nil || s.Trigger == nil || !missioncontrol.IsTypedInspectionTrigger(s.Trigger.Component.Key.Type)) {
		return errors.New("inspection destination requires commissioned native trigger")
	}
	if s.Proposal != nil && (s.Proposal.Validate() != nil || s.ControlContext == nil) {
		return errors.New("proposal requires commissioned control context")
	}
	if s.ControlContext == nil {
		return nil
	}
	if expected == nil || !missioncontrol.Equal(*s.ControlContext, *expected) {
		return errors.New("mission differs from commissioned control context")
	}
	c := s.ControlContext.Context
	if c.Validate() != nil || c.RunID != runID || s.Go2 != nil || s.Type != c.Type || s.Site != c.Site || s.Assignee != c.Assignee || s.Domain != mission.Simulation || s.Class != c.Class || s.Mode != c.Mode || s.Frame != c.Frame || s.Area.Min != c.Area.Min || s.Area.Max != c.Area.Max || !slices.Equal(s.RequiredSensors, c.RequiredSensors) || s.MinimumEnergy != c.MinimumEnergy || s.Fallback != c.Fallback || s.SpeedMPS != c.SpeedMPS || s.ToleranceM != c.ToleranceM || s.StopSpeedMPS != c.StopSpeedMPS || s.NotAfterUnixMS <= c.NotBeforeUnixMS || s.NotAfterUnixMS > c.NotAfterUnixMS || !inside(s.Area, s.Target) {
		return errors.New("mission outside commissioned control policy")
	}
	if s.Trigger != nil {
		if s.Trigger.Component.Key.Tenant != c.Tenant || s.Trigger.Component.Timing.ClockDomain != "simulation:"+c.RunID || s.Trigger.Validate(c.Site, c.Frame) != nil {
			return errors.New("trigger differs from commissioned scope")
		}
		if err := missioncontrol.ValidateNativeInspectionTarget(s.Trigger.Component, s.Target, s.InspectionDestination); err != nil {
			return err
		}
	}
	if s.Proposal != nil && s.Proposal.Audience != c.Audience {
		return errors.New("proposal audience differs from control context")
	}
	return nil
}

func validateControlConfiguration(cfg *Config) error {
	if cfg.ControlContext == nil {
		return nil
	}
	s := missioncontrol.Clone(*cfg.ControlContext)
	c := s.Context
	if c.Validate() != nil || len(s.Signature) != 64 || cfg.Domain != mission.Simulation || (cfg.Tenant != "" && cfg.Tenant != c.Tenant) || cfg.Site != c.Site || (cfg.Audience != "" && cfg.Audience != c.Audience) || cfg.RunID != c.RunID || cfg.Assignee != c.Assignee || cfg.Catalog[c.Type] != c.Class || cfg.Go2Binding != nil {
		return errors.New("invalid configured mission control context")
	}
	cfg.ControlContext = &s
	return nil
}

// CheckControlContext is safe on retained tasks too. It does not authorize
// execution and deliberately excludes expiry so stop/status remain available.
func (s *Store) CheckControlContext(spec Spec) error {
	return ValidateControlSpec(spec, s.cfg.ControlContext, s.cfg.RunID)
}
func (s *Store) ControlContext() *missioncontrol.SignedContext {
	if s.cfg.ControlContext == nil {
		return nil
	}
	c := missioncontrol.Clone(*s.cfg.ControlContext)
	return &c
}

// ControlProofTime constrains deadline from original issuance, not retry time.
func ControlProofTime(spec Spec, issued, expires int64) error {
	if spec.ControlContext == nil {
		return nil
	}
	c := spec.ControlContext.Context
	if issued < c.NotBeforeUnixMS || issued >= c.NotAfterUnixMS || spec.NotAfterUnixMS <= issued || spec.NotAfterUnixMS > expires || spec.NotAfterUnixMS-issued > c.MaxDurationMS {
		return errors.New("mission lifetime exceeds commissioned context")
	}
	return nil
}

// ValidateControlPayload checks JSON coordinates before fixed-size Go arrays can
// silently normalize null, missing or extra values in an operator payload.
func ValidateControlPayload(raw json.RawMessage, spec Spec) error {
	if spec.ControlContext == nil {
		return nil
	}
	canonical, err := json.Marshal(spec)
	if err != nil || !bytes.Equal(raw, canonical) {
		return errors.New("control payload must use canonical complete task fields")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return errors.New("invalid control payload")
	}
	target, err := missioncontrol.DecodeTarget(append(append([]byte(`{"target":`), fields["target"]...), '}'))
	if err != nil || target != spec.Target {
		return errors.New("invalid control target")
	}
	return nil
}
