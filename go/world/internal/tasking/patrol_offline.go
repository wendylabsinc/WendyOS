package tasking

import (
	"encoding/hex"
	"errors"
	"strings"
)

// PatrolOffline binds a prepared simulation mission to its complete world
// package, trusted application runtime, composed model and intended fresh run.
// Preparation's stationary boot is explicitly not an active mission source.
type PatrolOffline struct {
	KitID             string `json:"kit_id"`
	RunID             string `json:"run_id"`
	PackageSHA256     string `json:"package_sha256"`
	RuntimeSHA256     string `json:"runtime_sha256"`
	ModelSHA256       string `json:"model_sha256"`
	PreparationBootID string `json:"preparation_boot_id"`
}

func (o *PatrolOffline) Validate(s Spec) error {
	if o == nil {
		return nil
	}
	if len(o.KitID) != 28 || !strings.HasPrefix(o.KitID, "kit-") || o.RunID != "patrol-"+o.KitID || s.ID != o.RunID+"/patrol" || o.PreparationBootID == "" || len(o.PreparationBootID) > 128 {
		return errors.New("invalid prepared patrol scope")
	}
	if b, e := hex.DecodeString(o.KitID[4:]); e != nil || len(b) != 12 || hex.EncodeToString(b) != o.KitID[4:] {
		return errors.New("invalid kit identifier")
	}
	for _, v := range []string{o.PackageSHA256, o.RuntimeSHA256, o.ModelSHA256} {
		b, e := hex.DecodeString(v)
		if e != nil || len(b) != 32 || hex.EncodeToString(b) != v {
			return errors.New("prepared patrol needs exact lowercase digests")
		}
	}
	return nil
}

// ValidatePreparedPatrol checks the complete existing mission contract without
// starting an executor or capturing an active physics/work baseline.
func ValidatePreparedPatrol(s Spec) error {
	if s.Type != PatrolType || s.Patrol == nil || s.Patrol.Offline == nil {
		return errors.New("prepared approval requires an offline patrol")
	}
	return validateSpec(s)
}
