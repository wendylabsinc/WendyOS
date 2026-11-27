package client

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/proposalreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
	"net/url"
	"strconv"
)

func (c *Client) ProposalInbox(ctx context.Context, origin string, limit int, after string) (proposalreplica.List, error) {
	var out proposalreplica.List
	if !proposals.ValidID(origin) || limit < 1 || limit > 100 || (after != "" && !proposals.ValidID(after)) {
		return out, errors.New("invalid proposal inbox bounds")
	}
	q := url.Values{"origin": {origin}, "limit": {strconv.Itoa(limit)}}
	if after != "" {
		q.Set("after", after)
	}
	err := c.readStrict(ctx, "/v1/proposal-inbox?"+q.Encode(), &out)
	if err == nil {
		if out.Origin != origin {
			return out, errors.New("proposal inbox origin mismatch")
		}
		for _, v := range out.Proposals {
			if v.Origin != origin || v.Executable || !v.HistoricalProvenanceVerified {
				return out, errors.New("invalid historical proposal view")
			}
		}
	}
	return out, err
}
func (c *Client) RemoteProposal(ctx context.Context, origin, id string) (proposalreplica.View, error) {
	var out proposalreplica.View
	if !proposals.ValidID(origin) || !proposals.ValidID(id) {
		return out, errors.New("invalid proposal origin or ID")
	}
	err := c.readStrict(ctx, "/v1/proposal-inbox?"+url.Values{"origin": {origin}, "id": {id}}.Encode(), &out)
	if err == nil && (out.Origin != origin || out.Record.Proposal.ID != id || out.Executable || !out.HistoricalProvenanceVerified) {
		return out, errors.New("invalid historical proposal view")
	}
	return out, err
}
