package worldmcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
)

type proposalArgs struct {
	ID             string          `json:"id"`
	Assignee       string          `json:"assignee"`
	Type           string          `json:"type"`
	Parameters     json.RawMessage `json:"parameters"`
	Summary        string          `json:"summary"`
	NotAfterUnixMS int64           `json:"not_after_unix_ms"`
}
type proposalReceipt struct {
	ID                 string              `json:"id"`
	Sequence           uint64              `json:"sequence"`
	BodySHA256         string              `json:"body_sha256"`
	Assignee           string              `json:"assignee"`
	Type               string              `json:"type"`
	Class              mission.SafetyClass `json:"class"`
	NotAfterUnixMS     int64               `json:"not_after_unix_ms"`
	PendingHumanReview bool                `json:"pending_human_review"`
	AdapterValidated   bool                `json:"adapter_validated"`
	Expired            bool                `json:"expired"`
}

func (s *service) proposal(ctx context.Context, raw json.RawMessage) (*mcp.CallToolResult, error) {
	if !s.propose {
		return failure("proposal_scope_unavailable"), nil
	}
	var args proposalArgs
	if authority.DecodeStrictLimit(raw, &args, MaxInputBytes) != nil {
		return failure("invalid_arguments"), nil
	}
	catalog, err := s.readCatalog(ctx)
	if err != nil {
		return nil, err
	}
	var class mission.SafetyClass
	for _, entry := range catalog.Types {
		if args.Type == entry.Type {
			class = entry.Class
		}
	}
	if args.Assignee != catalog.Assignee || class == "" {
		return failure("proposal_outside_catalog"), nil
	}
	now := time.Now()
	if args.NotAfterUnixMS <= now.UnixMilli() || args.NotAfterUnixMS > now.Add(24*time.Hour).UnixMilli() || args.NotAfterUnixMS > s.notAfter {
		return failure("invalid_proposal_deadline"), nil
	}
	request := proposals.Request{Version: 1, ID: args.ID, Tenant: s.tenant, Site: s.site, RunID: catalog.RunID, Assignee: args.Assignee, Type: args.Type, Parameters: args.Parameters, Summary: args.Summary, NotAfterUnixMS: args.NotAfterUnixMS}
	body, err := json.Marshal(request)
	if err != nil {
		return failure("invalid_arguments"), nil
	}
	var reply json.RawMessage
	if err = s.backend.Do(ctx, "propose", http.MethodPost, "/v1/proposals", body, &reply); err != nil {
		// Delivery may have committed even when the reply was lost. This tool
		// does not retry, invent acceptance, or allocate a replacement ID.
		return failure("proposal_outcome_unknown"), nil
	}
	// Keep returned request proofs and exact body bytes out of the tool result.
	// They are checked only to bind the acknowledgement to this proposal.
	var view proposals.View
	if authority.DecodeStrictLimit(reply, &view, 1<<20) != nil {
		return failure("proposal_outcome_unknown"), nil
	}
	hash := sha256.Sum256(body)
	want, _ := json.Marshal(request)
	got, _ := json.Marshal(view.Record.Proposal)
	if view.AdapterValidated || view.Record.Sequence == 0 || view.Record.Class != class || view.Record.AcceptedAtUnixMS <= 0 ||
		view.Record.Kind != "agent" || view.Record.Principal != s.principal || !validIdentity(view.Record.GrantID) || view.Record.Source != s.source ||
		!bytes.Equal(got, want) || !bytes.Equal(view.Record.Body, body) || view.Record.BodySHA256 != hex.EncodeToString(hash[:]) {
		return failure("proposal_outcome_unknown"), nil
	}
	// Exact retries retain the original grant and proof even after this same
	// principal renews its credentials. Identity, source and body must still
	// match; the current grant's own validity is checked before and after I/O.
	receipt := proposalReceipt{ID: args.ID, Sequence: view.Record.Sequence, BodySHA256: view.Record.BodySHA256, Assignee: args.Assignee, Type: args.Type, Class: class, NotAfterUnixMS: args.NotAfterUnixMS, PendingHumanReview: true, AdapterValidated: false, Expired: view.Expired || time.Now().UnixMilli() >= args.NotAfterUnixMS}
	return bounded(Result{Stale: "unknown", Data: receipt}), nil
}
