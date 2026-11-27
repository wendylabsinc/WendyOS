// Package simbackend reads one explicitly commissioned local simulation replica.
// Its lab credential never grants Wendy production authority or hardware access.
package simbackend

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/production/readservice"
)

type Reader struct {
	client  *client.Client
	dataset readservice.Dataset
}

func New(config client.Config, dataset readservice.Dataset) (*Reader, error) {
	u, err := url.Parse(config.URL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("explicit numeric-loopback simulation URL required")
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port < 1 || port > 65535 {
		return nil, errors.New("simulation backend must be numeric loopback with a port")
	}
	g := config.Grant.Grant
	if dataset.Domain != "simulation" || dataset.ID == "" || dataset.RunID == "" || dataset.Frame.ID == "" || dataset.Frame.Revision == "" || g.Domain != "simulation" || g.Tenant != dataset.SourceTenant || g.Site != dataset.SourceSite || !config.AllowInsecureSimulation || g.Kind != "operator" {
		return nil, errors.New("simulation dataset and reader scope must match")
	}
	hasRead := false
	for _, scope := range g.Scopes {
		if scope != "read" && scope != "evidence-read" {
			return nil, errors.New("backend credential must have read-only scopes")
		}
		hasRead = hasRead || scope == "read"
	}
	if !hasRead || g.PublishAudience != "" || g.ExecuteAssignee != "" || g.TaskStatusAssignee != "" || g.TaskStatusAudience != "" {
		return nil, errors.New("backend reader must not carry write bindings")
	}
	c, err := client.New(config)
	if err != nil {
		return nil, err
	}
	return &Reader{client: c, dataset: dataset}, nil
}

func (r *Reader) Query(ctx context.Context, query readservice.Query) (readservice.Snapshot, error) {
	if query.Version != 1 || query.DatasetID != r.dataset.ID || query.Limit < 1 || query.Limit > readservice.MaxComponents {
		return readservice.Snapshot{}, errors.New("invalid simulation query")
	}
	value, err := r.client.Query(ctx)
	if err != nil {
		return readservice.Snapshot{}, err
	}
	out := readservice.Snapshot{Dataset: r.dataset, Cursor: value.Cursor, Components: make([]json.RawMessage, 0, min(len(value.Components), query.Limit))}
	// The upstream replica enforces expiry. The HTTP boundary validates every
	// returned component's scope, frame, clock and ancestry before releasing it.
	// The cursor identifies the underlying source snapshot, not only this frame.
	for _, component := range value.Components {
		// One replica legitimately contains site metadata and other frames.
		// Select the commissioned frame before applying the requested limit;
		// never relabel its records or skip validation of selected provenance.
		if component.Frame.ID != r.dataset.Frame.ID || component.Frame.Revision != r.dataset.Frame.Revision {
			continue
		}
		if len(out.Components) >= query.Limit {
			break
		}
		raw, err := json.Marshal(component)
		if err != nil {
			return readservice.Snapshot{}, err
		}
		out.Components = append(out.Components, raw)
	}
	return out, nil
}
