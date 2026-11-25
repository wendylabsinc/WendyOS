package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
)

var ErrSelectedViewChanged = errors.New("selected view changed; discard accumulated pages and restart")
var ErrSelectedCursorExpired = errors.New("selected cursor expired; restart without it")

func selectedError(err error) error {
	var status *HTTPStatusError
	if errors.As(err, &status) {
		switch status.StatusCode {
		case 409:
			return fmt.Errorf("%w: %v", ErrSelectedViewChanged, err)
		case 410:
			return fmt.Errorf("%w: %v", ErrSelectedCursorExpired, err)
		}
	}
	return err
}

func (c *Client) QuerySelected(ctx context.Context, request selection.Request) (selection.Response, error) {
	request, err := request.Normalize()
	if err != nil {
		return selection.Response{}, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return selection.Response{}, err
	}
	var raw json.RawMessage
	if err = c.Do(ctx, "read", http.MethodPost, "/v1/query", body, &raw); err != nil {
		return selection.Response{}, selectedError(err)
	}
	var response selection.Response
	if err = authority.DecodeStrictLimit(raw, &response, request.MaxBytes); err != nil {
		return selection.Response{}, err
	}
	if err = c.validateReplicaScope(response.ReplicaScope); err != nil {
		return selection.Response{}, err
	}
	if err = selection.ValidateResponse(response, request); err != nil {
		return selection.Response{}, err
	}
	if response.TimedOut {
		return selection.Response{}, errors.New("query response unexpectedly timed out")
	}
	return response, nil
}

func (c *Client) WatchSelected(ctx context.Context, request selection.WatchRequest) (selection.Response, error) {
	request, err := request.Normalize()
	if err != nil {
		return selection.Response{}, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return selection.Response{}, err
	}
	var raw json.RawMessage
	if err = c.Do(ctx, "read", http.MethodPost, "/v1/watch/selected", body, &raw); err != nil {
		return selection.Response{}, selectedError(err)
	}
	var response selection.Response
	if err = authority.DecodeStrictLimit(raw, &response, request.MaxBytes); err != nil {
		return selection.Response{}, err
	}
	page := selection.Request{Version: 1, Selection: request.Selection, PageSize: request.PageSize, MaxBytes: request.MaxBytes}
	if err = c.validateReplicaScope(response.ReplicaScope); err != nil {
		return selection.Response{}, err
	}
	if err = selection.ValidateResponse(response, page); err != nil {
		return selection.Response{}, err
	}
	if response.TimedOut && (request.Revision == "" || request.Revision != response.Revision) {
		return selection.Response{}, errors.New("watch timeout changed the selected revision")
	}
	return response, nil
}

// AreaSnapshot contains only complete, same-revision pages. It is a historical
// read response, never a replacement for current authority or a regional replica.
type AreaSnapshot struct {
	Version     int                  `json:"version"`
	Name        string               `json:"name"`
	Tenant      string               `json:"tenant"`
	Site        string               `json:"site"`
	Audience    string               `json:"audience"`
	Selection   selection.Selection  `json:"selection"`
	StartedAt   time.Time            `json:"started_at"`
	CompletedAt time.Time            `json:"completed_at"`
	Pages       []selection.Response `json:"pages"`
	Historical  bool                 `json:"historical"`
}

func (c *Client) ReadArea(ctx context.Context, name string, selected selection.Selection, pageSize int) (AreaSnapshot, error) {
	request, err := (selection.Request{Selection: selected, PageSize: pageSize, MaxBytes: 256 << 10}).Normalize()
	if err != nil {
		return AreaSnapshot{}, err
	}
	if request.Selection.Region == nil || strings.TrimSpace(name) == "" || len(name) > 120 {
		return AreaSnapshot{}, errors.New("area requires name and explicit region")
	}
	out := AreaSnapshot{Version: 1, Name: name, Tenant: c.config.Grant.Grant.Tenant, Site: c.config.Grant.Grant.Site, Audience: c.config.Audience, Selection: request.Selection, StartedAt: time.Now(), Pages: []selection.Response{}, Historical: true}
	revision := ""
	previous := ""
	count := 0
	for {
		page, err := c.QuerySelected(ctx, request)
		if err != nil {
			return AreaSnapshot{}, err
		}
		if revision != "" && revision != page.Revision {
			return AreaSnapshot{}, ErrSelectedViewChanged
		}
		revision = page.Revision
		for _, row := range page.Components {
			k := selection.Key(row)
			if k <= previous || row.Key.Tenant != out.Tenant || row.Key.Site != out.Site {
				return AreaSnapshot{}, errors.New("area page scope or ordering differs")
			}
			previous = k
			count++
		}
		out.Pages = append(out.Pages, page)
		raw, e := json.Marshal(out)
		if e != nil || len(out.Pages) > 32 || count > 512 || len(raw) > 2<<20 {
			return AreaSnapshot{}, errors.New("area exceeds complete snapshot bound")
		}
		if page.Complete {
			break
		}
		request.Cursor = page.NextCursor
	}
	out.CompletedAt = time.Now()
	return out, nil
}
func ValidateAreaSnapshot(s AreaSnapshot) error {
	selected, e := s.Selection.Normalize()
	if e != nil {
		return e
	}
	if s.Version != 1 || s.Name == "" || len(s.Name) > 120 || s.Tenant == "" || s.Site == "" || s.Audience == "" || !s.Historical || selected.Region == nil || len(s.Pages) == 0 || len(s.Pages) > 32 || s.StartedAt.IsZero() || s.CompletedAt.Before(s.StartedAt) {
		return errors.New("invalid area snapshot")
	}
	revision := ""
	previous := ""
	count := 0
	for i, page := range s.Pages {
		if e := selection.ValidateResponse(page, selection.Request{Selection: selected, PageSize: 128, MaxBytes: 256 << 10}); e != nil {
			return e
		}
		if page.Complete != (i == len(s.Pages)-1) || revision != "" && revision != page.Revision {
			return ErrSelectedViewChanged
		}
		revision = page.Revision
		for _, row := range page.Components {
			k := selection.Key(row)
			if k <= previous || row.Key.Tenant != s.Tenant || row.Key.Site != s.Site {
				return errors.New("area page scope/order")
			}
			previous = k
			count++
		}
	}
	raw, e := json.Marshal(s)
	if e != nil || len(raw) > 2<<20 || count > 512 {
		return errors.New("oversized area snapshot")
	}
	return nil
}
