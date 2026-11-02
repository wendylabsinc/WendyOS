package meshcatalog

import (
	"errors"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func (c *Catalog) manifestChange(r Record) (Record, error) {
	// mutation is held by the caller. Build and validate a complete replacement
	// before adopting intent, so an oversized update keeps the previous set.
	next := make(map[Key]localmesh.ManifestService, len(c.manifestOwn)+1)
	for k, v := range c.manifestOwn {
		next[k] = v
	}
	if r.Withdraw {
		delete(next, r.Key)
	} else {
		next[r.Key] = localmesh.ManifestService{Mesh: r.Key.Mesh, AppID: r.Key.AppID, ServiceID: r.Key.ServiceID,
			Type: r.Type, Instance: r.Instance, Port: r.HostPort, TXT: slices.Clone(r.TXT)}
	}
	keys := make([]Key, 0, len(next))
	for k := range next {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return lessKey(keys[i], keys[j]) })
	services := make([]localmesh.ManifestService, 0, len(keys))
	for _, k := range keys {
		services = append(services, next[k])
	}
	if reflect.DeepEqual(next, c.manifestOwn) {
		return r, nil
	}
	if err := c.manifestPublish(services); err != nil {
		return Record{}, err
	}
	c.manifestOwn = next
	return r, nil
}

func (c *Catalog) manifestRecords(now time.Time) []Record {
	view := c.manifestSnapshot()
	var out []Record
	for _, m := range view.Devices {
		if m.Withdraw || m.Expires <= now.UnixMilli() {
			continue
		}
		device := meshid.NewDevice(view.Self.Tenant(), m.Device)
		for _, service := range m.Services {
			if service.Mesh != c.mesh {
				continue
			}
			out = append(out, Record{Version: 1, Key: Key{service.Mesh, device.Principal(), service.AppID, service.ServiceID},
				Generation: m.Revision, Issued: m.Issued, Expires: m.Expires,
				Type: service.Type, Instance: service.Instance, HostPort: service.Port, TXT: slices.Clone(service.TXT)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return lessKey(out[i].Key, out[j].Key) })
	return out
}

// NewManifestCatalog retains local publication intent only. Remote service state
// is derived from the authenticated router directory, never a second blob cache.
func NewManifestCatalog(mesh string, device meshid.Device, authorize AuthorizePublication,
	publish func([]localmesh.ManifestService) error, snapshot func() localmesh.NodeSnapshot) (*Catalog, error) {
	if !labelPattern.MatchString(mesh) || !device.IsValid() || authorize == nil || publish == nil || snapshot == nil {
		return nil, errors.New("invalid manifest catalogue configuration")
	}
	return &Catalog{mesh: mesh, device: device, principal: device.Principal(), authorize: authorize,
		manifestPublish: publish, manifestSnapshot: snapshot, manifestOwn: make(map[Key]localmesh.ManifestService)}, nil
}
