package node

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

type SupervisionChallengeRequest struct {
	TaskID     string `json:"task_id"`
	TaskDigest string `json:"task_digest"`
	RequestID  string `json:"request_id"`
}

// SupervisorAuthority binds both sides of the exchange to current trust. A
// revoked gateway cannot sustain its old operator ACKs until their expiry.
func SupervisorAuthority(v *authority.Verifier, server authority.SignedGrant) func(json.RawMessage) (supervision.Ack, supervision.Operator, error) {
	return func(raw json.RawMessage) (supervision.Ack, supervision.Operator, error) {
		if _, err := v.VerifyGrant(server, "supervision-server"); err != nil {
			return supervision.Ack{}, supervision.Operator{}, err
		}
		return v.VerifySupervisionAckProof(raw)
	}
}

func SupervisorAuthorityContext(v *authority.Verifier, server authority.SignedGrant) func(context.Context, json.RawMessage) (supervision.Ack, supervision.Operator, error) {
	return func(ctx context.Context, raw json.RawMessage) (supervision.Ack, supervision.Operator, error) {
		if _, err := v.VerifyGrantContext(ctx, server, "supervision-server"); err != nil {
			return supervision.Ack{}, supervision.Operator{}, err
		}
		return v.VerifySupervisionAckProofContext(ctx, raw)
	}
}

// SupervisorAuthorityBatchContext freshly verifies a bounded group of active
// ACK proofs and its shared gateway in one durable authority transaction.
// Results belong only to this call; tasking must preserve every lease binding.
func SupervisorAuthorityBatchContext(v *authority.Verifier, server authority.SignedGrant) func(context.Context, []json.RawMessage) ([]supervision.Verification, error) {
	return func(ctx context.Context, proofs []json.RawMessage) ([]supervision.Verification, error) {
		return v.VerifySupervisionAcksContext(ctx, server, proofs)
	}
}

func (n *Node) supervisionRequest(w http.ResponseWriter, r *http.Request, path string) bool {
	w.Header().Set("Cache-Control", "no-store")
	if !n.cfg.SupervisionEnabled || n.cfg.Tasks == nil {
		http.Error(w, "supervision unavailable", 404)
		return false
	}
	if r.URL.Path != path || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || r.RequestURI != path {
		http.Error(w, "unsupported supervision target", 400)
		return false
	}
	if !method(w, r, http.MethodPost) {
		return false
	}
	return true
}

// Verify current server authority only after authenticating the caller. Grant
// verification checkpoints the trusted wall time in the durable replay ledger.
func (n *Node) supervisionServerAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if _, err := n.verifier.VerifyGrantContext(r.Context(), n.cfg.TaskStatusCredentials.Grant, "supervision-server"); err != nil {
		http.Error(w, "supervision server authority unavailable", 403)
		return false
	}
	return true
}
func supervisionError(w http.ResponseWriter, err error) {
	status := http.StatusConflict
	if errors.Is(err, tasking.ErrSupervisionLimit) {
		status = http.StatusTooManyRequests
	}
	if errors.Is(err, authority.ErrDenied) || errors.Is(err, authority.ErrStaleRevocations) {
		status = http.StatusForbidden
	}
	http.Error(w, "supervision exchange refused", status)
}
func (n *Node) supervisionChallenge(w http.ResponseWriter, r *http.Request) {
	if !n.supervisionRequest(w, r, "/v1/tasks/supervision/challenge") {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	id, ok := n.authorize(w, r, "supervise", body)
	if !ok {
		return
	}
	if id.Kind != "operator" || !id.Allows("stop") {
		http.Error(w, "operator supervision and stop authority required", 403)
		return
	}
	if !n.supervisionServerAuthorized(w, r) {
		return
	}
	var request SupervisionChallengeRequest
	if authority.DecodeStrict(body, &request) != nil {
		http.Error(w, "invalid challenge request", 400)
		return
	}
	c, err := n.cfg.Tasks.SupervisionChallengeContext(r.Context(), request.TaskID, request.TaskDigest, request.RequestID, supervision.Operator{Principal: id.Principal, KeySHA256: id.SigningKeySHA256, GrantID: id.GrantID, ValidUntilUnixMS: id.NotAfter.UnixMilli()})
	if err != nil {
		supervisionError(w, err)
		return
	}
	credentials := n.cfg.TaskStatusCredentials
	proof, err := authority.SignSupervisionChallenge(credentials.Grant, credentials.PrivateKey, c)
	if err == nil {
		_, err = n.verifier.VerifySupervisionChallengeContext(r.Context(), proof, credentials.Grant.Grant.Principal)
	}
	if err != nil {
		supervisionError(w, err)
		return
	}
	writeJSON(w, 200, proof)
}
func (n *Node) supervisionAck(w http.ResponseWriter, r *http.Request) {
	if !n.supervisionRequest(w, r, "/v1/tasks/supervision/ack") {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	id, ok := n.authorize(w, r, "supervise", body)
	if !ok {
		return
	}
	if !n.supervisionServerAuthorized(w, r) {
		return
	}
	ack, operator, err := n.verifier.VerifySupervisionAckProofContext(r.Context(), body)
	if err != nil || id.Kind != "operator" || !id.Allows("stop") || id.Principal != operator.Principal || id.SigningKeySHA256 != operator.KeySHA256 || id.GrantID != operator.GrantID || ack.Challenge.GatewayPrincipal != n.cfg.TaskStatusCredentials.Grant.Grant.Principal {
		http.Error(w, "acknowledgment authority rejected", 403)
		return
	}
	receipt, err := n.cfg.Tasks.AcknowledgeSupervisionContext(r.Context(), body)
	if err != nil {
		supervisionError(w, err)
		return
	}
	credentials := n.cfg.TaskStatusCredentials
	proof, err := authority.SignSupervisionReceipt(credentials.Grant, credentials.PrivateKey, receipt)
	if err == nil {
		_, err = n.verifier.VerifySupervisionReceiptContext(r.Context(), proof, credentials.Grant.Grant.Principal)
	}
	if err != nil {
		supervisionError(w, err)
		return
	}
	writeJSON(w, 200, proof)
}
