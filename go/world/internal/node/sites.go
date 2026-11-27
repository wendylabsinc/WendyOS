package node

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/zonepub"
)

// Every path that admits or exposes a reserved site record checks its origin's
// narrow authority. A relay's replicate grant cannot elevate an app publisher.
func (n *Node) verifyRecord(record authority.SignedRecord) (authority.Identity, error) {
	return n.verifyRecordContext(context.Background(), record)
}
func (n *Node) verifyRecordContext(ctx context.Context, record authority.SignedRecord) (authority.Identity, error) {
	identity, err := n.verifier.VerifyRecordContext(ctx, record)
	if err != nil {
		return identity, err
	}
	if record.Component.Key.Type == zonepub.Type {
		if identity.Kind != "operator" || !identity.Allows("site-publish") {
			return identity, authority.ErrDenied
		}
		if _, err := zonepub.Validate(record.Component); err != nil {
			return identity, err
		}
	}
	if record.Component.Key.Type == sitepub.Type {
		if identity.Kind != "operator" || !identity.Allows("site-publish") {
			return identity, errors.New("site publication requires operator site-publish authority")
		}
		publication, validationErr := sitepub.Validate(record.Component)
		if validationErr != nil {
			return identity, validationErr
		}
		now := time.Now()
		if now.Before(publication.ValidFrom) || !now.Before(publication.ValidUntil) {
			return identity, errors.New("site document is outside its validity interval")
		}
	}
	return identity, nil
}

func (n *Node) sites(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if _, ok := n.authorize(w, r, "read", nil); !ok {
		return
	}
	n.lock()
	defer n.mu.Unlock()
	records, err := n.store.Snapshot()
	if err != nil {
		http.Error(w, "site replica unavailable", http.StatusServiceUnavailable)
		return
	}
	publications := []authority.SignedRecord{}
	for _, c := range records {
		if c.Key.Type != sitepub.Type {
			continue
		}
		digest, err := recordDigest(c)
		if err != nil {
			http.Error(w, "site proof unavailable", http.StatusServiceUnavailable)
			return
		}
		proof, ok := n.proofs[digest]
		if !ok {
			http.Error(w, "site proof missing", http.StatusServiceUnavailable)
			return
		}
		proof.Component = c
		if _, err = n.verifyRecord(proof); err == nil {
			publications = append(publications, proof)
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Publications []authority.SignedRecord `json:"publications"`
	}{publications})
}
