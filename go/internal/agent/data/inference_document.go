package data

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/datadoc"
)

// InferenceDocumentVersion is the inference document schema this agent reads.
const InferenceDocumentVersion = 1

// inferenceRevisionSchema versions the field list digestInput covers. Adding a
// field there is a schema change: bump it with the field.
const inferenceRevisionSchema = 1

// InferenceDocument runs one detector on cameras and emits one event per
// arrival (spec §7). Its model fields are a campaign's inline inference block,
// with the same type and validation.
type InferenceDocument struct {
	datadoc.HeaderFields `yaml:",inline"`
	Sources              []CampaignSource `json:"sources" yaml:"sources"`
	CampaignInference    `yaml:",inline"`
}

func decodeInferenceDocument(_ datadoc.Header, contents []byte) (any, string, []string, error) {
	var document InferenceDocument
	if err := datadoc.DecodeStrict(contents, &document); err != nil {
		return nil, "", nil, err
	}
	if err := document.validate(); err != nil {
		return nil, "", nil, err
	}
	return document, document.revision(), nil, nil
}

func (d *InferenceDocument) validate() error {
	if len(d.Sources) == 0 {
		return errors.New("an inference must define at least one camera source")
	}
	for i, source := range d.Sources {
		if source.Camera == "" || strings.TrimSpace(source.Camera) != source.Camera || source.Audio != "" || source.ROS2 != "" || source.Telemetry || source.Calibration != "" || source.Capture != nil {
			return fmt.Errorf("sources[%d]: an inference reads cameras only: use `camera: <selector>`, without capture or calibration_revision", i)
		}
	}
	if d.Event == "" {
		d.Event = d.Name
	}
	return d.CampaignInference.validate()
}

func (d InferenceDocument) revision() string {
	cameras := make([]string, len(d.Sources))
	for i, source := range d.Sources {
		cameras[i] = source.Camera
	}
	canonical, _ := json.Marshal(map[string]any{
		"revision_schema": inferenceRevisionSchema,
		"version":         d.Version,
		"name":            d.Name,
		"owner":           d.Owner,
		"cameras":         cameras,
		"backend":         d.Backend,
		"model_file":      d.ModelFile,
		"model":           d.Model,
		"model_revision":  d.Revision,
		"labels":          d.Labels,
		"threshold":       d.Threshold,
		"rate":            d.Rate,
		"event":           d.Event,
		"clear_after":     d.ClearAfter,
		"cooldown":        d.Cooldown,
		"enabled":         d.IsEnabled(),
	})
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

// Campaign is the inference as the inference runner's job sees it: an armed
// plan with these cameras and this model, and nothing to capture.
func (d InferenceDocument) Campaign(revision string) Campaign {
	inference := d.CampaignInference
	return Campaign{Version: CampaignVersion, Name: d.Name, Sources: d.Sources, Inference: &inference, State: "armed", Revision: revision}
}
