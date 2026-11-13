package meshcatalog

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"sort"
	"sync"
	"time"
)

// Catalog retains authorized local intent. Remote records are projections of
// the router directory's verified manifests, with no independent blob cache.
type Catalog struct {
	mutation         sync.Mutex
	mesh             string
	device           meshid.Device
	principal        string
	authorize        AuthorizePublication
	manifestPublish  func([]localmesh.ManifestService) error
	manifestSnapshot func() localmesh.NodeSnapshot
	manifestOwn      map[Key]localmesh.ManifestService
}

func lessKey(a, b Key) bool {
	if a.Mesh != b.Mesh {
		return a.Mesh < b.Mesh
	}
	if a.Device != b.Device {
		return a.Device < b.Device
	}
	if a.AppID != b.AppID {
		return a.AppID < b.AppID
	}
	return a.ServiceID < b.ServiceID
}
func (c *Catalog) Publish(spec PublishSpec, now time.Time) (Record, error) {
	c.mutation.Lock()
	defer c.mutation.Unlock()
	if spec.AppID == GatewayAppID || spec.ServiceID == GatewayServiceID {
		return Record{}, errors.New("reserved gateway service identity")
	}
	if err := c.authorize(spec.AppID, spec.Type, spec.HostPort); err != nil {
		return Record{}, err
	}
	r, err := spec.record(Key{c.mesh, c.principal, spec.AppID, spec.ServiceID}, 1, now)
	if err != nil {
		return Record{}, err
	}
	return c.manifestChange(r)
}
func (c *Catalog) Remove(appID, serviceID string, now time.Time) (Record, error) {
	c.mutation.Lock()
	defer c.mutation.Unlock()
	if appID == GatewayAppID || serviceID == GatewayServiceID {
		return Record{}, errors.New("reserved gateway service identity")
	}
	return c.manifestChange(Record{Key: Key{c.mesh, c.principal, appID, serviceID}, Withdraw: true})
}
func (c *Catalog) PublishGatewayOffer(now time.Time) (Record, error) {
	c.mutation.Lock()
	defer c.mutation.Unlock()
	return c.manifestChange(Record{Version: 1, Key: Key{c.mesh, c.principal, GatewayAppID, GatewayServiceID}, Generation: 1, Issued: now.UnixMilli(), Expires: now.Add(MaxLease).UnixMilli()})
}
func (c *Catalog) WithdrawGatewayOffer(now time.Time) (Record, error) {
	c.mutation.Lock()
	defer c.mutation.Unlock()
	return c.manifestChange(Record{Key: Key{c.mesh, c.principal, GatewayAppID, GatewayServiceID}, Withdraw: true})
}
func (c *Catalog) Snapshot(now time.Time) []Record { return c.manifestRecords(now) }
func (c *Catalog) GatewayOffers(now time.Time) []Record {
	var out []Record
	for _, r := range c.Snapshot(now) {
		if IsGatewayOffer(r) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return lessKey(out[i].Key, out[j].Key) })
	return out
}
