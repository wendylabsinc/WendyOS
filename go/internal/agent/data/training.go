package data

import (
	"errors"
	"fmt"
	"math"
	"regexp"

	"gopkg.in/yaml.v3"
)

// CampaignTraining marks a campaign as training and simulator ready. Its mere
// presence is the signal; there is no boolean flag. It carries the control rate
// a trained policy runs at and the action stream that policy emits.
type CampaignTraining struct {
	RateHz float64                `json:"rate_hz" yaml:"rate_hz"`
	Action CampaignTrainingAction `json:"action" yaml:"action"`
}

// CampaignTrainingAction describes the command stream a trained policy writes.
// Channels are ordered and name the columns of the action vector; Range bounds
// each one; Unit is optional labelling.
type CampaignTrainingAction struct {
	Stream   string       `json:"stream" yaml:"stream"`
	Channels []string     `json:"channels" yaml:"channels"`
	Range    [][]float64  `json:"range" yaml:"range"`
	Unit     TrainingUnit `json:"unit,omitempty" yaml:"unit,omitempty"`
}

// TrainingUnit is an optional per-channel unit label. Authors may write a single
// string that applies to every channel, or a list with one entry per channel.
// It is stored as a slice so the two spellings validate and persist uniformly.
type TrainingUnit []string

// UnmarshalYAML accepts either a single scalar unit or a list of units. The
// document-wide KnownFields rejection is unaffected because this hook only
// governs this field's own node, not the surrounding mapping.
func (u *TrainingUnit) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var single string
		if err := node.Decode(&single); err != nil {
			return err
		}
		*u = TrainingUnit{single}
	case yaml.SequenceNode:
		var list []string
		if err := node.Decode(&list); err != nil {
			return err
		}
		*u = TrainingUnit(list)
	default:
		return errors.New("training.action.unit must be a string or a list of strings, one per channel")
	}
	return nil
}

// trainingStreamRE mirrors the recording-stream name rule in
// go/internal/shared/appconfig/recording.go (the unexported recordingName). The
// canonical regexp is not exported, so it is restated here; keep the two in sync.
var trainingStreamRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,23}$`)

func (t *CampaignTraining) validate() error {
	if t == nil {
		return nil
	}
	if math.IsNaN(t.RateHz) || math.IsInf(t.RateHz, 0) || t.RateHz <= 0 || t.RateHz > 1000 {
		return fmt.Errorf("training.rate_hz must be in (0, 1000]")
	}
	if !trainingStreamRE.MatchString(t.Action.Stream) {
		return fmt.Errorf("training.action.stream must be a lowercase letter followed by up to 23 letters, numbers, '-' or '_'")
	}
	if len(t.Action.Channels) == 0 {
		return fmt.Errorf("training.action.channels must contain at least one channel")
	}
	seen := map[string]bool{}
	for _, channel := range t.Action.Channels {
		if len(channel) == 0 || len(channel) > 128 || seen[channel] {
			return fmt.Errorf("training.action.channels must be unique, nonempty channel names")
		}
		seen[channel] = true
	}
	if len(t.Action.Range) != len(t.Action.Channels) {
		return fmt.Errorf("training.action.range must hold one [low, high] pair per channel")
	}
	for i, bounds := range t.Action.Range {
		if len(bounds) != 2 {
			return fmt.Errorf("training.action.range[%d] must be a [low, high] pair", i)
		}
		low, high := bounds[0], bounds[1]
		if math.IsNaN(low) || math.IsNaN(high) || math.IsInf(low, 0) || math.IsInf(high, 0) || low >= high {
			return fmt.Errorf("training.action.range[%d] must have finite bounds with low < high", i)
		}
	}
	// A single unit applies to every channel; otherwise one unit per channel.
	if len(t.Action.Unit) > 1 && len(t.Action.Unit) != len(t.Action.Channels) {
		return fmt.Errorf("training.action.unit must be a single unit or one unit per channel")
	}
	return nil
}
