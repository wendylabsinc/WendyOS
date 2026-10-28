package localmesh

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

// ControlHub shares the existing router pools and local service intent. There
// is no catalogue transport, fragmentation, event queue or per-link writer.
type ControlHub struct {
	certificates     *CertificatePool
	announcements    *netgossip.Pool
	manifestServices func([]ManifestService) error
}

func NewControlHub(pools ...*CertificatePool) *ControlHub {
	pool := NewCertificatePool()
	if len(pools) > 0 && pools[0] != nil {
		pool = pools[0]
	}
	return &ControlHub{certificates: pool, announcements: netgossip.NewPool()}
}
func (h *ControlHub) Certificates() *CertificatePool { return h.certificates }
func (h *ControlHub) Announcements() *netgossip.Pool { return h.announcements }
func (h *ControlHub) ReplaceServices(services []ManifestService) error {
	if h.manifestServices == nil {
		return errors.New("manifest publisher unavailable")
	}
	return h.manifestServices(cloneServices(services))
}
