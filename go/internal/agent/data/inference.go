package data

import (
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CampaignInference is executed by the agent, not by a user application.
// This first backend supports Transformers object-detection checkpoints.
type CampaignInference struct {
	Model      string   `json:"model" yaml:"model"`
	Revision   string   `json:"revision" yaml:"revision"`
	Labels     []string `json:"labels" yaml:"labels"`
	Threshold  float64  `json:"threshold" yaml:"threshold"`
	Rate       float64  `json:"rate" yaml:"rate"`
	Event      string   `json:"event" yaml:"event"`
	ClearAfter string   `json:"clear_after" yaml:"clear_after"`
	Cooldown   string   `json:"cooldown" yaml:"cooldown"`
	Enabled    *bool    `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

func (i *CampaignInference) IsEnabled() bool { return i != nil && (i.Enabled == nil || *i.Enabled) }
func (i *CampaignInference) ClearDuration() time.Duration {
	d, _ := time.ParseDuration(i.ClearAfter)
	return d
}
func (i *CampaignInference) CooldownDuration() time.Duration {
	d, _ := time.ParseDuration(i.Cooldown)
	return d
}

var modelRepositoryRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var modelRevisionRE = regexp.MustCompile(`^[a-f0-9]{40}$`)
var inferenceEventRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func (i *CampaignInference) validate() error {
	if i == nil {
		return nil
	}
	if strings.HasPrefix(i.Model, "https://huggingface.co/") {
		u, err := url.Parse(i.Model)
		if err != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("inference.model must be a Hugging Face repository URL without a query or fragment")
		}
		i.Model = strings.Trim(u.Path, "/")
	}
	if !modelRepositoryRE.MatchString(i.Model) {
		return fmt.Errorf("inference.model must be owner/repository or https://huggingface.co/owner/repository")
	}
	if !modelRevisionRE.MatchString(i.Revision) {
		return fmt.Errorf("inference.revision must pin a 40-character Hugging Face commit SHA")
	}
	if len(i.Labels) == 0 || len(i.Labels) > 32 {
		return fmt.Errorf("inference.labels must contain 1..32 labels")
	}
	seen := map[string]bool{}
	for _, label := range i.Labels {
		if len(label) == 0 || len(label) > 128 || strings.TrimSpace(label) != label || seen[label] {
			return fmt.Errorf("inference.labels must be unique, nonempty labels of at most 128 bytes")
		}
		seen[label] = true
	}
	if math.IsNaN(i.Threshold) || math.IsInf(i.Threshold, 0) || i.Threshold <= 0 || i.Threshold > 1 {
		return fmt.Errorf("inference.threshold must be in (0, 1]")
	}
	// Two records per result leaves room below the app record rate limit.
	if math.IsNaN(i.Rate) || math.IsInf(i.Rate, 0) || i.Rate <= 0 || i.Rate > 30 {
		return fmt.Errorf("inference.rate must be in (0, 30] frames per second per camera")
	}
	if !inferenceEventRE.MatchString(i.Event) {
		return fmt.Errorf("inference.event must use 1..128 letters, numbers, '.', '-' or '_'")
	}
	for name, raw := range map[string]string{"clear_after": i.ClearAfter, "cooldown": i.Cooldown} {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 || d > 24*time.Hour {
			return fmt.Errorf("inference.%s must be a positive duration of at most 24h", name)
		}
	}
	return nil
}

// InferenceStatus is live state, never part of the campaign revision or plan.
type InferenceStatus struct {
	State             string            `json:"state"`
	Error             string            `json:"error,omitempty"`
	Sources           map[string]string `json:"sources,omitempty"`
	NotificationError string            `json:"notification_error,omitempty"`
}

// InferenceDirectory is outside the episode quota and upload tree.
func (m *Manager) InferenceDirectory() string {
	return filepath.Join(filepath.Dir(m.root), "inference")
}
