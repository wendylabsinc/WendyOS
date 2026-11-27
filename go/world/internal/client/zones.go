package client

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/internal/zonepub"
	"net/http"
	"time"
)

// Zones verifies every original proof against caller-pinned trust. A response is
// this replica's current view, never proof of site-wide completeness or absence.
func (c *Client) Zones(ctx context.Context, v *authority.Verifier) (zonepub.Reply, error) {
	var raw json.RawMessage
	if err := c.Do(ctx, "read", http.MethodGet, "/v1/zones", nil, &raw); err != nil {
		return zonepub.Reply{}, err
	}
	var reply zonepub.Reply
	if err := authority.DecodeStrictLimit(raw, &reply, zonepub.MaxReplyBytes); err != nil {
		return reply, err
	}
	if reply.Records == nil || len(reply.Records) > 10000 {
		return reply, errors.New("invalid zone list")
	}
	seen := map[world.Key]bool{}
	for _, r := range reply.Records {
		if seen[r.Component.Key] {
			return reply, errors.New("duplicate zone key")
		}
		seen[r.Component.Key] = true
		if _, err := zonepub.Verify(r, v, time.Now()); err != nil {
			return reply, err
		}
	}
	return reply, nil
}

func SelectZone(reply zonepub.Reply, digest string, source world.Source, now time.Time) (zonepub.Selection, error) {
	if !zonepub.ValidDigest(digest) || source.Validate() != nil {
		return zonepub.Selection{}, errors.New("exact digest and source required")
	}
	for _, r := range reply.Records {
		if r.Component.Key.Entity == zonepub.Prefix+digest && r.Component.Key.Source == source {
			p, err := zonepub.Validate(r.Component)
			if err != nil {
				return zonepub.Selection{}, err
			}
			state := "available_at_replica"
			if r.Component.Tombstone {
				state = "deleted_at_replica"
			}
			return zonepub.Selection{Publication: p, Record: r, CheckedAt: now, State: state}, nil
		}
	}
	return zonepub.Selection{}, errors.New("zone revision unavailable at this replica; global absence is unknown")
}
