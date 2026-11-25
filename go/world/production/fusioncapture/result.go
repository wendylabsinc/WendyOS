package fusioncapture

import (
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// BuildPublication wraps the current fusion app's hazard without inventing an
// output source, epoch or publication sequence. Inputs are the already verified
// exact records used for evaluation, with their host-computed live deadlines.
// This pure builder does not establish current authority/freshness or pin any
// proof; the receiving agent must recheck those conditions and commit retention.
func BuildPublication(result fusion.Result, inputs []resultrecord.ValidatedInput, stream string, retainUntil time.Time) (serviceorigin.PublishRequest, error) {
	hazard := result.Hazard
	if result.State != fusion.HazardDetected || hazard == nil || !sensing.DigestValid(result.InputDigest) || result.InputDigest != hazard.InputDigest || len(inputs) == 0 || len(inputs) != len(hazard.Inputs) || len(inputs) > resultrecord.MaxInputs || retainUntil.IsZero() {
		return serviceorigin.PublishRequest{}, errors.New("complete detected hazard and exact verified inputs required")
	}
	byProof := make(map[string]resultrecord.ValidatedInput, len(inputs))
	for _, input := range inputs {
		if _, duplicate := byProof[input.Reference.EnvelopeSHA256]; duplicate {
			return serviceorigin.PublishRequest{}, errors.New("duplicate host input")
		}
		byProof[input.Reference.EnvelopeSHA256] = input
	}
	envelope := resultrecord.Result{Version: resultrecord.Version, ResultSchema: fusion.HazardType, RetainUntilUnixMS: retainUntil.UnixMilli()}
	publication := serviceorigin.PublishRequest{ComponentType: resultrecord.ComponentType, Stream: stream, Provenance: world.Provenance{Origin: world.Inferred, AppVersion: fusion.AppVersion, ModelID: fusion.ModelID, ModelVersion: fusion.ModelVersion}}
	seen := map[string]bool{}
	var oldest, until time.Time
	for i, used := range hazard.Inputs {
		if used.Capture == nil || used.Replay != nil || seen[used.Capture.ProofSHA256] {
			return serviceorigin.PublishRequest{}, errors.New("hazard inputs must identify distinct live agent capture proofs")
		}
		seen[used.Capture.ProofSHA256] = true
		input, ok := byProof[used.Capture.ProofSHA256]
		if !ok || input.Reference.Validate() != nil || input.Reference.Key != input.Component.Key || input.Reference.Version != input.Component.Version || used.Source != input.Reference.Key.Source || used.Version != input.Reference.Version {
			return serviceorigin.PublishRequest{}, errors.New("hazard input disagrees with verified proof source or version")
		}
		capture, err := sensorrecord.FromComponent(input.Component)
		if err != nil {
			return serviceorigin.PublishRequest{}, err
		}
		sample, err := capture.NormalizedSample()
		if err != nil {
			return serviceorigin.PublishRequest{}, err
		}
		var originalEvidence *sensing.EvidenceRef
		if capture.SensorLink != nil {
			original, err := capture.Sample()
			if err != nil {
				return serviceorigin.PublishRequest{}, err
			}
			originalEvidence = original.Evidence
		}
		start := input.Component.Timing.SampleTime
		if sample.Audio != nil {
			start = start.Add(-time.Duration(sample.Audio.DurationNS))
		}
		if !reflect.DeepEqual(used.Sample, sample) || used.SampleID != sample.ID || used.SensorID != sample.SensorID || used.Kind != sample.Kind || !reflect.DeepEqual(used.Evidence, sample.Evidence) || used.Capture.SceneBootID != capture.SceneBootID || !used.Capture.SimulationAnchor.Equal(time.Unix(0, capture.SimulationAnchorUnixNS)) || !reflect.DeepEqual(used.Capture.OriginalEvidence, originalEvidence) || !used.SampleTime.Equal(input.Component.Timing.SampleTime) || !used.CaptureStart.Equal(start) || used.ValidUntil.IsZero() || used.ValidUntil.After(input.SampleValidUntil) {
			return serviceorigin.PublishRequest{}, errors.New("hazard input changed original capture metadata or validity")
		}
		if i == 0 {
			publication.Entity = sample.RunID + "/possible-gas-hazard"
			publication.Frame = input.Component.Frame
			publication.Timing.ClockDomain = input.Component.Timing.ClockDomain
		}
		if oldest.IsZero() || start.Before(oldest) {
			oldest = start
		}
		if until.IsZero() || used.ValidUntil.Before(until) {
			until = used.ValidUntil
		}
		envelope.Inputs = append(envelope.Inputs, input.Reference)
		leaf := world.Input{ID: input.Reference.ID(), Origin: input.Component.Provenance.Origin}
		if simulation := input.Component.Provenance.Simulation; simulation != nil {
			copy := *simulation
			leaf.Simulation = &copy
		}
		publication.Provenance.Inputs = append(publication.Provenance.Inputs, leaf)
	}
	if !until.Equal(hazard.ValidUntil) || !until.After(oldest) {
		return serviceorigin.PublishRequest{}, errors.New("hazard deadline disagrees with required inputs")
	}
	publication.Timing.SampleTime, publication.Timing.ErrorKnown = oldest, true
	publication.TTL = until.Sub(oldest)
	var err error
	envelope.ResultJSON, err = fusion.PublicationJSON(hazard)
	if err != nil {
		return serviceorigin.PublishRequest{}, err
	}
	publication.Payload, err = resultrecord.Encode(envelope)
	if err != nil {
		return serviceorigin.PublishRequest{}, err
	}
	validated, err := resultrecord.ValidatePublication(publication, inputs)
	if err != nil {
		return serviceorigin.PublishRequest{}, err
	}
	// Carry the exact host-computed physical input cap to remote readers.
	// Retention, result signing time and simulation time cannot replace it.
	deadline, err := json.Marshal(validated.PhysicalValidUntil.UTC())
	if err != nil {
		return serviceorigin.PublishRequest{}, err
	}
	body := envelope.ResultJSON
	body = append(body[:len(body)-1:len(body)-1], []byte(`,"physical_valid_until":`)...)
	body = append(body, deadline...)
	envelope.ResultJSON = append(body, '}')
	publication.Payload, err = resultrecord.Encode(envelope)
	if err != nil {
		return serviceorigin.PublishRequest{}, err
	}
	return publication, nil
}
