package authority

import (
	"crypto/ed25519"

	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
)

func SignMissionControlContext(c missioncontrol.Context, root ed25519.PrivateKey) (missioncontrol.SignedContext, error) {
	s := missioncontrol.SignedContext{Context: c}
	if c.Validate() != nil {
		return s, ErrDenied
	}
	var err error
	s.Signature, err = sign(root, "mission-control-context", c)
	return s, err
}

// VerifyMissionControlContext authenticates commissioned policy, including for
// retained history. Current validity and exact configured scope are admission
// checks. In particular, this function never makes stop depend on context time.
func VerifyMissionControlContext(s missioncontrol.SignedContext, root ed25519.PublicKey) error {
	if s.Context.Validate() != nil || !verify(root, "mission-control-context", s.Context, s.Signature) {
		return ErrDenied
	}
	return nil
}
func (v *Verifier) VerifyMissionControlContext(s missioncontrol.SignedContext) error {
	if err := VerifyMissionControlContext(s, v.cfg.RootPublicKey); err != nil {
		return err
	}
	c := s.Context
	if v.cfg.Domain != "simulation" || c.Tenant != v.cfg.Tenant || c.Site != v.cfg.Site || c.Audience != v.cfg.Audience {
		return ErrDenied
	}
	return nil
}
