package client

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/subscription"
)

func (c *Client) validateReplicaScope(scope *selection.ReplicaScope) error {
	if scope == nil {
		return nil
	}
	p := subscription.Policy{UnknownLocation: scope.UnknownLocation, Version: 1, Tenant: scope.Tenant, Site: scope.Site, Selection: scope.Selection, RequiredTypes: scope.RequiredTypes}
	normalized, err := p.Normalize()
	if err != nil {
		return err
	}
	if !scope.Partial || scope.Tenant != c.config.Grant.Grant.Tenant || scope.Site != c.config.Grant.Grant.Site || normalized.Digest() != scope.PolicySHA256 {
		return errors.New("invalid partial replica scope")
	}
	return nil
}
func (c *Client) ReplicaStatus(ctx context.Context) (subscription.Status, error) {
	var raw json.RawMessage
	var out subscription.Status
	if err := c.Do(ctx, "read", "GET", "/v1/replica/status", nil, &raw); err != nil {
		return out, err
	}
	if err := authority.DecodeStrictLimit(raw, &out, 1<<20); err != nil {
		return out, err
	}
	if out.Version != 1 || out.Tenant != c.config.Grant.Grant.Tenant || out.Site != c.config.Grant.Grant.Site || out.ObservedAt.IsZero() || out.RetainedRecords < 0 || out.LimitRecords < 1 || out.RetainedRecords+out.RetainedFences > out.LimitRecords || out.RetainedFences < 0 || out.UnknownLocationRecords < 0 || out.UnknownLocationRecords > out.RetainedRecords || out.RetainedTombstones < 0 || out.RetainedTombstones > out.RetainedRecords || out.ExpiredRecords < 0 || out.ExpiredRecords > out.RetainedRecords || out.Peers == nil || len(out.Peers) > 32 || out.SyncSemantics == "" {
		return subscription.Status{}, errors.New("invalid replica status")
	}
	if out.Policy == nil {
		if out.Mode != "full" || out.PolicySHA256 != "" {
			return subscription.Status{}, errors.New("invalid full replica status")
		}
	} else {
		p, err := out.Policy.Normalize()
		if err != nil {
			return subscription.Status{}, err
		}
		if out.Mode != "partial" || p.Tenant != out.Tenant || p.Site != out.Site || p.Digest() != out.PolicySHA256 {
			return subscription.Status{}, errors.New("invalid partial replica status")
		}
		out.Policy = &p
	}
	for _, peer := range out.Peers {
		if peer.Audience == "" || peer.LastAttempt.IsZero() || (peer.LastOutcome != "running" && peer.LastOutcome != "complete" && peer.LastOutcome != "failed" && peer.LastOutcome != "deferred") {
			return subscription.Status{}, errors.New("invalid peer traversal status")
		}
		if peer.SourcePartial {
			decoded, e := hex.DecodeString(peer.SourcePolicySHA256)
			if !peer.SourceScopeKnown || e != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != peer.SourcePolicySHA256 {
				return subscription.Status{}, errors.New("invalid peer partial scope")
			}
		} else if peer.SourcePolicySHA256 != "" {
			return subscription.Status{}, errors.New("invalid peer full scope")
		}

	}
	return out, nil
}
