package data

import (
	"fmt"
	"strings"
)

// RuleInputPrefix is where the grid streams a campaign rule may read live: the
// agent's own camera grids, agent/camera/grid/<source>. The rule steps work on
// any wendy.data.v1.Grid; this release runs them where the agent produces the
// grid, on every frame its decoder makes, which only the agent's own camera
// grids allow.
const RuleInputPrefix = "agent/camera/grid/"

// maxCampaignRules bounds the rules of one campaign.
const maxCampaignRules = 16

// RuleStatus is a campaign rule's live state, reported by campaign inspect.
type RuleStatus struct {
	// State is running while the rule gets frames, waiting_for_frames while
	// its camera is absent or its decoder cannot run, error when this agent
	// cannot run the rule, and stopped when no rule runtime runs.
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	// MarkersStream is the live stream of the rule's areas, when it has one.
	MarkersStream string `json:"markers_stream,omitempty"`
	// LastFrameAt is the time of the last frame the rule evaluated.
	LastFrameAt string `json:"last_frame_at,omitempty"`
	// Events counts the events the rule raised since it started; LastEvent
	// and LastEventAt name the last one and its frame's time.
	Events      uint64 `json:"events,omitempty"`
	LastEvent   string `json:"last_event,omitempty"`
	LastEventAt string `json:"last_event_at,omitempty"`
}

// RuleStatuses holds RuleStatus by rule name.
type RuleStatuses map[string]RuleStatus

func (c Campaign) validateRules() error {
	if len(c.Rules) > maxCampaignRules {
		return fmt.Errorf("rules may list at most %d rules", maxCampaignRules)
	}
	seen := map[string]bool{}
	for i, rule := range c.Rules {
		label := fmt.Sprintf("rules[%d]", i)
		if rule.Name != "" {
			label += " (" + rule.Name + ")"
		}
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		source, ok := strings.CutPrefix(rule.Input, RuleInputPrefix)
		if !ok || source == "" {
			return fmt.Errorf("%s: input must name one of the agent's camera grids, %s<source>, as `wendy data streams` lists it, for example %sv4l2:/dev/video2", label, RuleInputPrefix, RuleInputPrefix)
		}
		if seen[rule.Name] {
			return fmt.Errorf("%s: another rule is named %s; rule names must be unique within a campaign", label, rule.Name)
		}
		seen[rule.Name] = true
	}
	return nil
}
