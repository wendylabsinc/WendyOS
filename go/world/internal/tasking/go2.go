package tasking

import (
	"errors"
	"regexp"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const Go2GoToType = "wendy.go2.nav.go_to.v1"

// Go2Binding is part of the operator-signed instruction. An unsigned import
// observation cannot populate authority implicitly. Alignment and profile
// hashes cover exact separately commissioned configuration bytes.
type Go2Binding struct {
	Schema           string         `json:"schema"`
	PackageSHA256    string         `json:"package_sha256"`
	ConversionSHA256 string         `json:"conversion_sha256"`
	LossSHA256       string         `json:"loss_sha256"`
	ProjectRevision  string         `json:"project_revision"`
	ProjectID        string         `json:"project_id"`
	VMName           string         `json:"vm_name"`
	SourceDigest     string         `json:"source_digest"`
	PolicyBundle     string         `json:"policy_bundle"`
	WorldEpoch       uint64         `json:"world_epoch"`
	RunID            string         `json:"run_id"`
	Frame            world.FrameRef `json:"frame"`
	AlignmentSHA256  string         `json:"alignment_sha256"`
	ProfileSHA256    string         `json:"profile_sha256"`
}

var go2Digest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var go2ProjectID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var go2Token = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func (b Go2Binding) Validate() error {
	if b.Schema != "wendy.world.go2-execution.v1" || !go2ProjectID.MatchString(b.ProjectID) || !go2Token.MatchString(b.VMName) || !go2Token.MatchString(b.RunID) || b.PolicyBundle != "go2-moe-cts-164k-0.6715-v1" || b.WorldEpoch == 0 || b.WorldEpoch > 1<<53-1 || b.Frame.Validate() != nil || !go2Token.MatchString(b.Frame.ID) || !go2Token.MatchString(b.Frame.Revision) {
		return errors.New("invalid commissioned Go2 simulation binding")
	}
	if len(b.SourceDigest) != 71 || b.SourceDigest[:7] != "sha256:" || !go2Digest.MatchString(b.SourceDigest[7:]) {
		return errors.New("invalid Go2 runtime source digest")
	}
	for _, digest := range []string{b.PackageSHA256, b.ConversionSHA256, b.LossSHA256, b.ProjectRevision, b.AlignmentSHA256, b.ProfileSHA256} {
		if !go2Digest.MatchString(digest) {
			return errors.New("invalid Go2 configuration digest")
		}
	}
	return nil
}

func validateGo2Shape(s Spec) error {
	if s.Type != Go2GoToType {
		if s.Go2 != nil {
			return errors.New("Go2 binding requires its declared catalog type")
		}
		return nil
	}
	if s.Go2 == nil || s.Go2.Validate() != nil || s.Go2.Frame != s.Frame || s.Domain != mission.Simulation || s.Class != mission.Move || s.Mode != mission.Autonomous || s.Trigger != nil || s.Fallback != mission.Stop {
		return errors.New("Go2 requires an explicitly bound autonomous simulation MOVE with STOP fallback")
	}
	if !finite(s.MinimumEnergy) || s.MinimumEnergy != 0 || len(s.RequiredSensors) != 2 || !((s.RequiredSensors[0] == "odometry" && s.RequiredSensors[1] == "local-obstacles") || (s.RequiredSensors[1] == "odometry" && s.RequiredSensors[0] == "local-obstacles")) {
		return errors.New("Go2 requires odometry and local obstacles; energy is not observed")
	}
	if !finite(s.Target[1]) || s.Target[1] != 0 || s.SpeedMPS <= 0 || s.SpeedMPS > .35 || s.ToleranceM > .35 || s.StopSpeedMPS <= 0 || s.StopSpeedMPS > .1 || s.Area.Max[0]-s.Area.Min[0] > 8 || s.Area.Max[2]-s.Area.Min[2] > 8 {
		return errors.New("Go2 target or operating area exceeds the commissioned planar profile")
	}
	return nil
}

// ValidateGo2Spec checks both the signed navigation limits and their exact local
// commissioning pins. It never derives pins from a live unsigned status query.
func ValidateGo2Spec(s Spec, expected *Go2Binding, runID string) error {
	if err := validateSpec(s); err != nil {
		return err
	}
	if s.Type != Go2GoToType || expected == nil || expected.Validate() != nil || expected.RunID != runID || s.Go2 == nil || *s.Go2 != *expected {
		return errors.New("signed Go2 instruction differs from commissioned execution scope")
	}
	return nil
}

func validateGo2Configuration(cfg Config) error {
	class, enabled := cfg.Catalog[Go2GoToType]
	if !enabled {
		if cfg.Go2Binding != nil {
			return errors.New("Go2 binding configured without its task catalog")
		}
		return nil
	}
	if len(cfg.Catalog) != 1 || class != mission.Move || cfg.Domain != mission.Simulation || cfg.Go2Binding == nil || cfg.Go2Binding.Validate() != nil || cfg.Go2Binding.RunID != cfg.RunID || cfg.MaxLease > 500*time.Millisecond || cfg.MaxTelemetryAge > 250*time.Millisecond {
		return errors.New("Go2 catalog requires one commissioned autonomous simulation scope")
	}
	return nil
}
