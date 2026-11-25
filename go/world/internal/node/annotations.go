package node

import (
	"errors"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotations"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

var (
	ErrPublisherEpoch     = errors.New("publisher must allocate and use current epoch; pending signed records cannot be rebased")
	ErrAnnotationConflict = errors.New("annotation ID is immutable; create a new draft ID")
	ErrAnnotationFrame    = errors.New("annotation frame revision is not active; create a new draft in the current frame")
)

func (n *Node) annotationEpoch(w http.ResponseWriter, r *http.Request) {
	n.epochFor(w, r, "annotation-write")
}

func (n *Node) annotations(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	identity, ok := n.authorize(w, r, "annotation-write", body)
	if !ok {
		return
	}
	var record authority.SignedRecord
	if err := authority.DecodeStrict(body, &record); err != nil {
		http.Error(w, "invalid annotation record", http.StatusBadRequest)
		return
	}
	note, err := annotations.Validate(record.Component)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if identity.Source != record.Component.Key.Source || identity.Principal != note.AuthorPrincipal {
		http.Error(w, "annotation author/source mismatch", http.StatusForbidden)
		return
	}
	// Request verification binds this audience to the receiving gateway. The
	// origin grant must name that same owner, even across grant renewals.
	// Replica repair separately accepts records owned by other gateways.
	if record.Grant.Grant.PublishAudience != identity.PublishAudience {
		http.Error(w, "annotation origin belongs to another gateway", http.StatusForbidden)
		return
	}
	if !time.Now().Add(record.Component.Timing.ErrorBound).Before(time.UnixMilli(note.ExpiresAtUnixMS)) {
		http.Error(w, "annotation draft expired", http.StatusGone)
		return
	}
	result, err := n.applyContext(r.Context(), record, true)
	if err != nil {
		// Storage/proof failures must remain retryable. The schema was validated
		// above; only known admission failures are terminal client errors.
		status := http.StatusServiceUnavailable
		if errors.Is(err, world.ErrWrongScope) || errors.Is(err, world.ErrWrongSource) || errors.Is(err, world.ErrFutureSample) || errors.Is(err, world.ErrUnknownFreshness) || errors.Is(err, world.ErrSynthetic) || errors.Is(err, world.ErrConflict) {
			status = http.StatusBadRequest
		}
		if errors.Is(err, ErrPublisherEpoch) || errors.Is(err, ErrAnnotationConflict) || errors.Is(err, ErrAnnotationFrame) {
			status = http.StatusConflict
		}
		if errors.Is(err, authority.ErrDenied) || errors.Is(err, authority.ErrStaleRevocations) {
			status = http.StatusForbidden
		}
		http.Error(w, "annotation rejected: "+err.Error(), status)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Result    world.Result    `json:"result"`
		Component world.Component `json:"component"`
	}{result, record.Component})
}

// Annotation UUID ownership survives schema revisions; Type is not identity.
func annotationIdentity(key world.Key) world.Key { key.Type = ""; return key }
