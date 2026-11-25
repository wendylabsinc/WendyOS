package go2project

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const BindingSchema = "wendy.world.go2-project-binding.v1"

type Binding struct {
	Schema                  string      `json:"schema"`
	Domain                  string      `json:"domain"`
	Authority               string      `json:"authority"`
	Operational             bool        `json:"operational"`
	OperationalAuthority    bool        `json:"operational_authority"`
	RuntimeInstanceVerified bool        `json:"runtime_instance_verified"`
	BoundUnixMS             int64       `json:"bound_unix_ms"`
	Pins                    Pins        `json:"pins"`
	Projection              Projection  `json:"projection"`
	Observation             Observation `json:"observation"`
}
type Check struct {
	Schema                  string       `json:"schema"`
	Domain                  string       `json:"domain"`
	Authority               string       `json:"authority"`
	Operational             bool         `json:"operational"`
	OperationalAuthority    bool         `json:"operational_authority"`
	RuntimeInstanceVerified bool         `json:"runtime_instance_verified"`
	CheckedUnixMS           int64        `json:"checked_unix_ms"`
	BindingSHA256           string       `json:"binding_sha256"`
	Valid                   bool         `json:"valid"`
	Reason                  string       `json:"reason"`
	Observation             *Observation `json:"observation,omitempty"`
}

func Bind(ctx context.Context, packagePath, projectDir, reportPath string, pins Pins) (Binding, error) {
	projection, err := ValidateConversion(packagePath, projectDir, reportPath)
	if err != nil {
		return Binding{}, err
	}
	if pins.ProjectRevision != projection.ProjectRevision {
		return Binding{}, errors.New("expected project revision differs from projected files")
	}
	observation, err := Inspect(ctx, pins)
	if err != nil {
		return Binding{}, err
	}
	return Binding{Schema: BindingSchema, Domain: "simulation", Authority: "unsigned-local-observation", Operational: false, BoundUnixMS: time.Now().UnixMilli(), Pins: pins, Projection: projection, Observation: observation}, nil
}

// CheckBinding reports invalidation without changing the prior binding. Runtime
// assertions are untrusted observations, not a signed service identity or grant.
func CheckBinding(ctx context.Context, bindingPath, packagePath, projectDir, reportPath string) (Check, error) {
	result := Check{Schema: "wendy.world.go2-project-binding-check.v1", Domain: "simulation", Authority: "unsigned-local-observation", Operational: false, CheckedUnixMS: time.Now().UnixMilli(), Reason: "binding_invalid"}
	data, err := ReadFile(bindingPath, 2<<20)
	if err != nil {
		return result, err
	}
	result.BindingSHA256 = hash(data)
	var prior Binding
	if strict(data, &prior, 2<<20) != nil || !requiredFields(data, "schema", "domain", "authority", "operational", "operational_authority", "bound_unix_ms", "pins", "projection", "observation") || prior.Schema != BindingSchema || prior.Domain != "simulation" || prior.Authority != "unsigned-local-observation" || prior.Operational || prior.OperationalAuthority || prior.RuntimeInstanceVerified || prior.BoundUnixMS <= 0 || prior.Pins.Validate() != nil || prior.Observation.WorldEpoch != prior.Pins.WorldEpoch || !hexDigest(prior.Observation.StatusSHA256) || prior.Observation.SimulationTime < 0 || prior.Observation.ProjectEpoch == 0 {
		return result, errors.New("invalid prior observational binding")
	}
	projection, err := ValidateConversion(packagePath, projectDir, reportPath)
	if err != nil {
		result.Reason = "conversion_invalid"
		return result, err
	}
	currentJSON, _ := json.Marshal(projection)
	priorJSON, _ := json.Marshal(prior.Projection)
	if !jsonEqual(currentJSON, priorJSON) || projection.ProjectRevision != prior.Pins.ProjectRevision {
		result.Reason = "conversion_changed"
		return result, errors.New("conversion or projected files changed")
	}
	observation, err := Inspect(ctx, prior.Pins)
	if err != nil {
		result.Reason = "runtime_unavailable_or_changed"
		return result, err
	}
	result.Observation = &observation
	if observation.ProjectEpoch != prior.Observation.ProjectEpoch || observation.SimulationTime < prior.Observation.SimulationTime {
		result.Reason = "runtime_reset_or_regressed"
		return result, errors.New("runtime project epoch or simulation time changed")
	}
	result.Valid = true
	result.Reason = "binding_unchanged"
	return result, nil
}

// DecodeBinding is also useful to read a public prior report without network use.
func DecodeBinding(data []byte) (Binding, error) {
	var b Binding
	err := strict(data, &b, 2<<20)
	return b, err
}
