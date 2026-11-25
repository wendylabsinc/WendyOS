package patrolkit

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"time"
)

const Audience = "go2-patrol-simulation"
const Executor = "patrol/executor"

// Verifier uses a local catalog pin, never a root selected by the kit payload.
// It verifies original authority on every call. It cannot sign or renew it.
func Verifier(approval Approval, pin []byte, now func() time.Time) (*authority.Verifier, error) {
	if len(pin) != ed25519.PublicKeySize || !bytes.Equal(pin, approval.RootPublicKey) || !approval.Fixture || approval.Hardware {
		return nil, errors.New("offline kit does not match the local simulation authority pin")
	}
	v, e := authority.NewVerifier(authority.Config{RootPublicKey: ed25519.PublicKey(pin), Tenant: "simulation", Site: "studio-01", Domain: "simulation", Audience: Audience, Now: now, Replay: authority.NewMemoryReplay(128), MaxRevocationAge: 25 * time.Hour, MaxGrantLifetime: 25 * time.Hour})
	if e == nil {
		e = v.UpdateRevocations(approval.Revocations)
	}
	return v, e
}
func VerifyApproval(approval Approval, entry Entry, now time.Time) (tasking.Spec, *authority.Verifier, error) {
	var spec tasking.Spec
	v, e := Verifier(approval, entry.RootPublicKey, func() time.Time { return now })
	if e != nil {
		return spec, nil, e
	}
	identity, e := v.VerifyInstruction(approval.Instruction, "mission")
	if e != nil {
		return spec, nil, e
	}
	if e = authority.DecodeStrict(approval.Instruction.Payload, &spec); e != nil {
		return spec, nil, e
	}
	raw, _ := json.Marshal(spec)
	if !bytes.Equal(raw, approval.Instruction.Payload) {
		return spec, nil, errors.New("prepared mission has noncanonical or unsupported fields")
	}
	if spec.NotAfterUnixMS > identity.NotAfter.UnixMilli() {
		return spec, nil, errors.New("mission outlives original signed authority")
	}
	if e = tasking.ValidatePreparedPatrol(spec); e != nil {
		return spec, nil, e
	}
	p := entry.Profile
	if p.Schema != Schema || !token.MatchString(p.ID) || p.Name == "" || len(p.Name) > 120 || p.RunID != "patrol-"+p.ID || p.PreparedUnixMS <= 0 || p.PreparedUnixMS > now.UnixMilli() || p.ExpiresUnixMS <= now.UnixMilli() || p.ExpiresUnixMS != spec.NotAfterUnixMS || p.Frame != spec.Frame || p.Runtime.Validate() != nil || spec.Patrol == nil || spec.Patrol.Offline == nil || spec.Type != tasking.PatrolType || spec.Site != "studio-01" {
		return spec, nil, errors.New("invalid/expired prepared profile or mission scope")
	}
	o := spec.Patrol.Offline
	if o.Validate(spec) != nil || o.KitID != p.ID || o.RunID != p.RunID || o.PackageSHA256 != p.PackageSHA256 || o.ModelSHA256 != p.ModelSHA256 || o.RuntimeSHA256 != p.Runtime.Digest() || o.PreparationBootID != p.PreparationBootID {
		return spec, nil, errors.New("offline profile differs from original signed mission")
	}
	return spec, v, nil
}
