package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/proposalreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
)

func (n *Node) configureProposalInbox() error {
	if n.cfg.ProposalInbox != nil && !n.cfg.ProposalInbox.MatchesScope(n.cfg.Tenant, n.cfg.Site) {
		return errors.New("proposal inbox scope mismatch")
	}
	if n.cfg.ProposalExportCredentials != nil {
		c := n.cfg.ProposalExportCredentials
		if n.cfg.Proposals == nil || n.cfg.Audience == "" || c.Grant.Grant.Kind != "device" {
			return errors.New("proposal export requires local proposals and device credentials")
		}
		if _, err := n.verifier.VerifyGrant(c.Grant, "replicate"); err != nil {
			return err
		}
		if _, err := authority.SignRequest(c.Grant, c.PrivateKey, n.cfg.Audience, "replicate", []byte("probe"), "proposal-export-validation", time.Now(), time.Minute); err != nil {
			return err
		}
	}
	return nil
}
func inboxError(w http.ResponseWriter, err error) {
	code := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, proposalreplica.ErrInvalid):
		code = 400
	case errors.Is(err, proposalreplica.ErrConflict):
		code = 409
	case errors.Is(err, proposalreplica.ErrCapacity):
		code = 507
	case errors.Is(err, proposals.ErrNotFound):
		code = 404
	}
	http.Error(w, "proposal inbox: "+err.Error(), code)
}
func (n *Node) proposalInboxRead(w http.ResponseWriter, r *http.Request) {
	if n.cfg.ProposalInbox == nil {
		http.NotFound(w, r)
		return
	}
	if !method(w, r, http.MethodGet) {
		return
	}
	if _, ok := n.authorize(w, r, "read", nil); !ok {
		return
	}
	q, err := strictQuery(r, "origin", "id", "limit", "after")
	if err != nil || q["origin"] == "" {
		inboxError(w, proposalreplica.ErrInvalid)
		return
	}
	if q["id"] != "" {
		if q["limit"] != "" || q["after"] != "" {
			inboxError(w, proposalreplica.ErrInvalid)
			return
		}
		v, e := n.cfg.ProposalInbox.Get(r.Context(), q["origin"], q["id"])
		if e != nil {
			inboxError(w, e)
			return
		}
		writeJSON(w, 200, v)
		return
	}
	limit := 20
	if q["limit"] != "" {
		limit, err = strconv.Atoi(q["limit"])
		if err != nil {
			inboxError(w, proposalreplica.ErrInvalid)
			return
		}
	}
	v, err := n.cfg.ProposalInbox.List(r.Context(), q["origin"], limit, q["after"])
	if err != nil {
		inboxError(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (n *Node) proposalReplica(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if _, ok := n.authorize(w, r, "replicate", nil); !ok {
		return
	}
	q, e := strictQuery(r, "origin", "after")
	after, err := strconv.ParseUint(q["after"], 10, 64)
	if e != nil || err != nil || !proposals.ValidID(q["origin"]) {
		inboxError(w, proposalreplica.ErrInvalid)
		return
	}
	origin := q["origin"]
	if origin != n.cfg.Audience || n.cfg.Proposals == nil || n.cfg.ProposalExportCredentials == nil {
		if n.cfg.ProposalInbox == nil {
			http.NotFound(w, r)
			return
		}
		p, e := n.cfg.ProposalInbox.Export(r.Context(), origin, after)
		if e != nil {
			inboxError(w, e)
			return
		}
		writeJSON(w, 200, p)
		return
	}
	creds := n.cfg.ProposalExportCredentials
	if _, e = n.verifier.VerifyGrantContext(r.Context(), creds.Grant, "replicate"); e != nil {
		inboxError(w, e)
		return
	}
	records, more, e := n.cfg.Proposals.Export(after, 16)
	if e != nil {
		inboxError(w, e)
		return
	}
	p := proposalreplica.Page{Origin: origin, After: after, Through: after, More: more, Records: []proposalreplica.Receipt{}}
	for _, record := range records {
		body, e := proposalreplica.ReceiptBody(origin, record)
		if e != nil {
			inboxError(w, e)
			return
		}
		nonce := make([]byte, 24)
		if _, e = rand.Read(nonce); e != nil {
			inboxError(w, e)
			return
		}
		proof, e := authority.SignRequest(creds.Grant, creds.PrivateKey, origin, "replicate", body, hex.EncodeToString(nonce), time.Now(), time.Minute)
		if e != nil {
			inboxError(w, e)
			return
		}
		candidate := p
		candidate.Records = append(append([]proposalreplica.Receipt{}, p.Records...), proposalreplica.Receipt{Record: record, OriginProof: proof})
		candidate.Through = record.Sequence
		b, _ := json.Marshal(candidate)
		if len(b)+1 > proposalreplica.MaxPageBytes {
			p.More = true
			break
		}
		p = candidate
	}
	if len(records) > 0 && len(p.Records) == 0 {
		inboxError(w, proposalreplica.ErrCapacity)
		return
	}
	writeJSON(w, 200, p)
}

// SyncProposalInbox repairs read-only history only. Origin approval and task
// dispatch deliberately never consult this store.
func (n *Node) SyncProposalInbox(ctx context.Context, peer Peer, credentials Credentials) error {
	if n.cfg.ProposalInbox == nil {
		return nil
	}
	var failures []error
	for origin := range n.cfg.ProposalInbox.Origins() {
		after, e := n.cfg.ProposalInbox.Cursor(origin)
		if e != nil {
			return e
		}
		endpoint, e := url.Parse(peer.URL)
		if e != nil || endpoint.Scheme != "http" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return errors.New("invalid simulation peer URL")
		}
		endpoint.Path = "/v1/proposal-inbox/replica"
		endpoint.RawQuery = url.Values{"origin": {origin}, "after": {strconv.FormatUint(after, 10)}}.Encode()
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if e != nil {
			return e
		}
		if e = AuthorizeRequest(req, credentials, peer.Audience, "replicate", nil); e != nil {
			return e
		}
		response, e := n.client.Do(req)
		if e != nil {
			failures = append(failures, e)
			continue
		}
		b, e := io.ReadAll(io.LimitReader(response.Body, proposalreplica.MaxPageBytes+1))
		response.Body.Close()
		if e != nil || response.StatusCode != 200 || len(b) > proposalreplica.MaxPageBytes {
			failures = append(failures, fmt.Errorf("proposal inbox peer %s returned %d", peer.Audience, response.StatusCode))
			continue
		}
		var page proposalreplica.Page
		if authority.DecodeStrictLimit(b, &page, proposalreplica.MaxPageBytes) != nil || page.Origin != origin || page.After != after {
			failures = append(failures, proposalreplica.ErrInvalid)
			continue
		}
		if e = n.cfg.ProposalInbox.Import(ctx, page, peer.Audience); e != nil {
			failures = append(failures, e)
		}
	}
	return errors.Join(failures...)
}
