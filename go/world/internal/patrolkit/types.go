// Package patrolkit retains one approved, simulation-only offline patrol.
// Cached bytes do not create authority: activation uses a separately pinned
// local catalog entry and the original signed instruction and expiry.
package patrolkit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/internal/worldpackage"
	"regexp"
)

const Schema = "wendy.sim.offline-patrol-kit.v1"
const MaxRuntimeBytes = 32 << 20
const MaxKitBytes = 128 << 20
const MaxEntries = 32

var token = regexp.MustCompile(`^kit-[a-f0-9]{24}$`)

func Digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func validDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

type Runtime struct {
	PhysicsImage    string `json:"physics_image"`
	NavigationImage string `json:"navigation_image"`
	ExecutorSHA256  string `json:"executor_sha256"`
	ViewerSHA256    string `json:"viewer_sha256"`
}

func (r Runtime) Digest() string { raw, _ := json.Marshal(r); return Digest(raw) }
func (r Runtime) Validate() error {
	for _, s := range []string{r.PhysicsImage, r.NavigationImage} {
		if len(s) != 71 || s[:7] != "sha256:" || !validDigest(s[7:]) {
			return errors.New("offline runtime requires exact cached image digests")
		}
	}
	if !validDigest(r.ExecutorSHA256) || !validDigest(r.ViewerSHA256) {
		return errors.New("offline runtime requires exact application hashes")
	}
	return nil
}

type Profile struct {
	Schema            string         `json:"schema"`
	ID                string         `json:"id"`
	Name              string         `json:"name"`
	RunID             string         `json:"run_id"`
	PackageSHA256     string         `json:"package_sha256"`
	ModelSHA256       string         `json:"model_sha256"`
	PreparationBootID string         `json:"preparation_boot_id"`
	Frame             world.FrameRef `json:"frame"`
	Runtime           Runtime        `json:"runtime"`
	PreparedUnixMS    int64          `json:"prepared_unix_ms"`
	ExpiresUnixMS     int64          `json:"expires_unix_ms"`
}
type Approval struct {
	Instruction   authority.SignedInstruction `json:"instruction"`
	RootPublicKey []byte                      `json:"root_public_key"`
	Revocations   authority.SignedRevocations `json:"revocations"`
	Fixture       bool                        `json:"fixture"`
	Hardware      bool                        `json:"hardware"`
}
type Manifest struct {
	Schema string              `json:"schema"`
	ID     string              `json:"id"`
	Files  []worldpackage.File `json:"files"`
}

// Entry is the local approval pin. It is never loaded from the untrusted kit.
type Entry struct {
	Profile       Profile `json:"profile"`
	KitSHA256     string  `json:"kit_sha256"`
	RootPublicKey []byte  `json:"root_public_key"`
}
type Verified struct {
	Entry    Entry
	Approval Approval
	Spec     tasking.Spec
	Bundle   worldpackage.Bundle
	Files    map[string][]byte
}
type Claim struct {
	Schema        string `json:"schema"`
	ID            string `json:"id"`
	KitSHA256     string `json:"kit_sha256"`
	RunID         string `json:"run_id"`
	Output        string `json:"output"`
	ClaimedUnixMS int64  `json:"claimed_unix_ms"`
}
type Inspection struct {
	Profile   Profile               `json:"profile"`
	KitSHA256 string                `json:"kit_sha256"`
	State     string                `json:"state"`
	Reason    string                `json:"reason,omitempty"`
	Bytes     int64                 `json:"bytes"`
	FileCount int                   `json:"file_count"`
	Policy    *tasking.PatrolPolicy `json:"policy,omitempty"`
}
