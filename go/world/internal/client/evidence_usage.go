package client

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
)

func (c *Client) EvidenceUsage(ctx context.Context) (evidence.Usage, error) {
	var raw json.RawMessage
	var out evidence.Usage
	if err := c.Do(ctx, "evidence-read", "POST", "/v1/evidence/usage", []byte(`{}`), &raw); err != nil {
		return out, err
	}
	if err := authority.DecodeStrictLimit(raw, &out, 16<<10); err != nil {
		return evidence.Usage{}, err
	}
	if out.Tenant != c.config.Grant.Grant.Tenant || out.Site != c.config.Grant.Grant.Site || out.Validate() != nil {
		return evidence.Usage{}, errors.New("invalid evidence usage response")
	}
	return out, nil
}
