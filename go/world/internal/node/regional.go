package node

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

// Generic records may outlive the grant that originally signed them. Advancing
// a regional storage fence requires current publication authority as well.
func (n *Node) regionalCurrent(ctx context.Context, r authority.SignedRecord, id authority.Identity) (authority.Identity, error) {
	if !n.store.Subscription().Regional() {
		return id, nil
	}
	current, err := n.verifier.VerifyGrantContext(ctx, r.Grant, "publish")
	if err != nil {
		return authority.Identity{}, err
	}
	if current.NotAfter.Before(id.NotAfter) {
		id.NotAfter = current.NotAfter
	}
	return id, nil
}

// Caller owns n.mu except during construction. Old proofs cover precommit
// crashes; prune only after the committed world no longer needs those bytes.
func (n *Node) pruneRegionalProofs() error {
	if !n.store.Subscription().Regional() {
		return nil
	}
	records, err := n.store.Records()
	if err != nil {
		return err
	}
	keep := map[string]authority.SignedRecord{}
	for _, c := range records {
		d, e := recordDigest(c)
		if e != nil {
			return e
		}
		p, ok := n.proofs[d]
		if !ok {
			return errors.New("regional retained record has no origin proof")
		}
		keep[d] = p
	}
	if len(keep) == len(n.proofs) {
		return nil
	}
	if err = n.saveProofs(keep); err != nil {
		return err
	}
	n.proofs = keep
	return nil
}
