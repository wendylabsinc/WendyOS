package authority

import (
	"context"
	"time"
)

// VerifyProposalHistory verifies exact non-executable historical bytes. It does
// not consume a nonce or authorize admission, execution, or a new HTTP request.
// Current revocation and historical admission are deliberately not asserted.
// operation is restricted to proposer proofs and read-only replica receipts.
func (v *Verifier) VerifyProposalHistory(ctx context.Context, r SignedRequest, operation, audience string, body []byte) (Identity, error) {
	hold := v.lockContext(ctx)
	defer v.mu.Unlock()
	defer hold.End()
	if ctx.Err() != nil || (operation != "propose" && operation != "replicate") || audience == "" || len(audience) > 256 || len(body) > v.cfg.MaxBodyBytes || r.Version != Version || r.Audience != audience || r.Operation != operation || r.BodySHA256 != hash(body) || !validNonce(r.Nonce) || !validWindow(r.IssuedAtUnixMS, r.ExpiresAtUnixMS, v.cfg.MaxRequestLifetime) || r.IssuedAtUnixMS > v.cfg.Now().UnixMilli() {
		return Identity{}, ErrDenied
	}
	g := r.Grant.Grant
	if (operation == "propose" && g.Kind != "agent" && g.Kind != "operator") || (operation == "replicate" && g.Kind != "device") || validateGrant(g, v.cfg.MaxGrantLifetime) != nil || !verify(v.cfg.RootPublicKey, "grant", g, r.Grant.Signature) || !verify(g.PublicKey, "request", requestPayload(r), r.Signature) || g.Tenant != v.cfg.Tenant || g.Site != v.cfg.Site || g.Domain != "simulation" || v.cfg.Domain != "simulation" || !contains(g.Scopes, operation) || r.IssuedAtUnixMS < g.NotBeforeUnixMS || r.IssuedAtUnixMS >= g.NotAfterUnixMS {
		return Identity{}, ErrDenied
	}
	id := grantIdentity(g)
	id.NotAfter = time.UnixMilli(r.ExpiresAtUnixMS)
	return id, nil
}
