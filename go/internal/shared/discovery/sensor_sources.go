package discovery

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// DiscoverSensorSources browses the local network for SensorLink sources:
// WendyOS agents on _wendyos._udp and Wendy Lite boards on _wendy-lite._tcp
// that advertise the "sensors" capability. The agent runs it to answer
// WendySensorPairingService.DiscoverSensorSources from the device's own
// network; the CLI runs it on its own network only when the device's agent
// predates that RPC. Both callers therefore see the same shape.
//
// Both services are browsed concurrently for at most timeout (zero uses the
// default). Sightings of one source that differ only in address (a board
// answering on several interfaces) collapse into a single entry whose
// Addresses lists each address.
//
// exclude, when non-nil, drops matching sightings before they count. The
// agent passes its own advertisement: a browse on the device hears that
// first, and if it counted, the WendyOS browse would settle and end before
// other agents had a chance to answer.
//
// Every field comes from mDNS TXT records and is an unauthenticated hint. The
// pairing transport authenticates the source when it connects.
//
// The error is ctx's error when ctx ended before the browse finished, in which
// case the result may be incomplete. Otherwise it is non-nil only when a
// browse failed and no source was found, so a broken mDNS stack is not
// mistaken for an empty network. A browse that fails while the other finds
// sources is not reported: the result still holds those sources.
func DiscoverSensorSources(ctx context.Context, timeout time.Duration, exclude func(models.LANDevice) bool) ([]models.LANDevice, error) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	var (
		wg                 sync.WaitGroup
		agents             []models.LANDevice
		lite               []MDNSService
		agentsErr, liteErr error
	)
	// CollectLAN waits for its backend before returning, so agentsErr is
	// written before wg.Wait returns.
	opts := StreamOptions{OnBackendError: func(err error) { agentsErr = err }}
	if exclude != nil {
		opts.Exclude = excludeFunc(exclude)
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Pass ctx itself, not one bounded by timeout, so the scan's own timer
		// ends it. A ctx that ends first still returns what was confirmed.
		agents, _ = CollectLAN(ctx, opts, timeout)
	}()
	go func() {
		defer wg.Done()
		lite, liteErr = BrowseMDNSServices(ctx, WendyLiteServiceType, timeout)
	}()
	wg.Wait()

	sightings := make([]models.LANDevice, 0, len(agents)+len(lite))
	sightings = append(sightings, agents...)
	for _, svc := range lite {
		if d := LANDeviceFromWendyLiteService(svc); exclude == nil || !exclude(d) {
			sightings = append(sightings, d)
		}
	}
	sources := mergeSensorSources(sightings)
	if err := ctx.Err(); err != nil {
		return sources, err
	}
	if len(sources) == 0 {
		if agentsErr != nil {
			return nil, fmt.Errorf("browsing %s: %w", wendyServiceType, agentsErr)
		}
		if liteErr != nil {
			return nil, fmt.Errorf("browsing %s: %w", WendyLiteServiceType, liteErr)
		}
	}
	return sources, nil
}

// excludeFunc is a LANFilter whose answer never changes.
type excludeFunc func(models.LANDevice) bool

func (f excludeFunc) Exclude(d models.LANDevice) bool { return f(d) }
func (excludeFunc) Changed() <-chan struct{}          { return nil }

// mergeSensorSources keeps the sightings that advertise the "sensors"
// capability, in order, merging those that agree on every identity field and
// differ only in address. Sightings that disagree on any identity field stay
// separate, so a spoofed advertisement cannot hide a real one.
func mergeSensorSources(sightings []models.LANDevice) []models.LANDevice {
	var out []models.LANDevice
	index := make(map[string]int)
	for _, d := range sightings {
		if !d.Sensorlink {
			continue
		}
		key := sensorSourceIdentity(d)
		i, seen := index[key]
		if !seen {
			d.Addresses = unionAddresses([]string{d.IPAddress}, d.Addresses...)
			index[key] = len(out)
			out = append(out, d)
			continue
		}
		merged := &out[i]
		if merged.IPAddress == "" {
			merged.IPAddress = d.IPAddress
		}
		merged.Addresses = unionAddresses(merged.Addresses, append([]string{d.IPAddress}, d.Addresses...)...)
	}
	return out
}

func sensorSourceIdentity(d models.LANDevice) string {
	return fmt.Sprintf("%t\x00%s\x00%s\x00%s\x00%d\x00%t\x00%d\x00%d\x00%s\x00%s\x00%s",
		d.WendyLite, d.ID, d.DisplayName, d.Hostname, d.Port, d.IsMTLS,
		d.AssetID, d.OrgID, d.TenantUUID, d.PrincipalURI, strings.Join(d.Caps, ","))
}

// MergeSensorSources combines ordinary LAN and authenticated mesh sightings
// using the existing source identity rules. Addresses with different ports or
// identity metadata remain distinct so each endpoint keeps its own identity.
func MergeSensorSources(sightings []models.LANDevice) []models.LANDevice {
	return mergeSensorSources(sightings)
}
