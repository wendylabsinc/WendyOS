package meshcatalog

import (
	"fmt"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

// LANBrowser reads unsigned DNS-SD only on the same enabled physical links
// used for Wendy's LAN carrier. It never accepts packets from an app bridge.
type LANBrowser struct {
	cache    *LANCache
	ethernet bool
	wifi     bool
	mu       sync.RWMutex
	current  []localmesh.PhysicalLANInterface
}

func NewLANBrowser(ethernet, wifi bool) *LANBrowser {
	return &LANBrowser{cache: NewLANCache(), ethernet: ethernet, wifi: wifi}
}

func (b *LANBrowser) Snapshot(now time.Time) []LANService {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	interfaces := append([]localmesh.PhysicalLANInterface(nil), b.current...)
	b.mu.RUnlock()
	observed := b.cache.Snapshot(interfaces, now)
	// The same responder may be heard on Ethernet and Wi-Fi. Publish one
	// DNS-SD RRset while either source lease is live; prefer the cheaper link.
	selected := make(map[string]LANService, len(observed))
	for _, service := range observed {
		key := service.Source.String() + "/" + service.Protocol + "/" + fmt.Sprint(service.Port) + "/" + projectionKey(service.Records[0])
		old, exists := selected[key]
		if !exists || service.Interface.Cost < old.Interface.Cost ||
			service.Interface.Cost == old.Interface.Cost && service.Interface.Index < old.Interface.Index {
			selected[key] = service
		}
	}
	result := make([]LANService, 0, len(selected))
	for _, service := range selected {
		result = append(result, service)
	}
	return result
}
