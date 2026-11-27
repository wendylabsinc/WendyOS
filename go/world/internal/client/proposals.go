package client

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

func (c *Client) readStrict(ctx context.Context, path string, out any) error {
	var raw json.RawMessage
	if err := c.Do(ctx, "read", http.MethodGet, path, nil, &raw); err != nil {
		return err
	}
	return authority.DecodeStrictLimit(raw, out, 1<<20)
}

func (c *Client) TaskCatalog(ctx context.Context) (tasking.Catalog, error) {
	var out tasking.Catalog
	err := c.readStrict(ctx, "/v1/tasks/catalog", &out)
	return out, err
}

func (c *Client) TaskStatus(ctx context.Context, id string) (tasking.Record, error) {
	var out tasking.Record
	if id == "" || len(id) > 256 {
		return out, errors.New("invalid task ID")
	}
	err := c.readStrict(ctx, "/v1/tasks/status?"+url.Values{"task_id": {id}}.Encode(), &out)
	return out, err
}

// ProposeTask submits a suggestion only. An error after transmission may leave
// its outcome unknown. Callers can inspect the retained ID; never auto-retry
// with a changed ID or deadline, and never treat a receipt as task admission.
func (c *Client) ProposeTask(ctx context.Context, request proposals.Request) (proposals.View, error) {
	var out proposals.View
	body, err := json.Marshal(request)
	if err != nil {
		return out, err
	}
	var raw json.RawMessage
	if err = c.Do(ctx, "propose", http.MethodPost, "/v1/proposals", body, &raw); err != nil {
		return out, err
	}
	err = authority.DecodeStrictLimit(raw, &out, proposals.MaxPageBytes)
	return out, err
}

func (c *Client) Proposals(ctx context.Context, limit int, after string) (proposals.Page, error) {
	var out proposals.Page
	if limit < 1 || limit > 100 || (after != "" && !proposals.ValidID(after)) {
		return out, errors.New("invalid proposal page bounds")
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if after != "" {
		query.Set("after", after)
	}
	err := c.readStrict(ctx, "/v1/proposals?"+query.Encode(), &out)
	return out, err
}

func (c *Client) Proposal(ctx context.Context, id string) (proposals.View, error) {
	var out proposals.View
	if !proposals.ValidID(id) {
		return out, errors.New("invalid proposal ID")
	}
	err := c.readStrict(ctx, "/v1/proposals/"+id, &out)
	return out, err
}

func (c *Client) Watch(ctx context.Context, cursor string, waitMS int) (Snapshot, error) {
	var out Snapshot
	b, err := hex.DecodeString(cursor)
	if waitMS < 0 || waitMS > 5000 || (cursor != "" && (err != nil || len(b) != 32)) {
		return out, errors.New("invalid watch bounds")
	}
	err = c.readStrict(ctx, "/v1/watch?"+url.Values{"cursor": {cursor}, "timeout_ms": {strconv.Itoa(waitMS)}}.Encode(), &out)
	if err == nil {
		err = c.validateReplicaScope(out.ReplicaScope)
	}
	return out, err
}
