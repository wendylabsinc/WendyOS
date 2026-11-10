package commands

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

type meshMetadataRow struct {
	event      discovery.LANEvent
	cancel     context.CancelFunc
	next       time.Time
	order      uint64
	advertised bool
}
type meshMetadataResult struct {
	row    *meshMetadataRow
	device models.LANDevice
	err    error
}

// Mesh presence remains visible while bounded metadata RPCs run. Route loss
// cancels its probe; an old reply cannot resurrect or overwrite a new route.
func probeMeshDiscovery(ctx context.Context, events <-chan discovery.LANEvent, probe discovery.LANProber, refresh time.Duration) <-chan discovery.LANEvent {
	out := make(chan discovery.LANEvent)
	go func() {
		defer close(out)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		rows := map[string]*meshMetadataRow{}
		results := make(chan meshMetadataResult)
		active := 0
		var order uint64
		tick := time.NewTicker(refresh)
		defer tick.Stop()
		send := func(event discovery.LANEvent) bool {
			select {
			case out <- event:
				return true
			case <-ctx.Done():
				return false
			}
		}
		schedule := func() {
			if ctx.Err() != nil {
				return
			}
			// All destinations can share one BLE hop. Concurrent TLS handshakes
			// compete for that hop and repeatedly time out before metadata arrives.
			if active != 0 {
				return
			}
			var row *meshMetadataRow
			for _, candidate := range rows {
				if candidate.advertised || candidate.cancel != nil || time.Now().Before(candidate.next) {
					continue
				}
				if row == nil || candidate.next.Before(row.next) || candidate.next.Equal(row.next) && candidate.order < row.order {
					row = candidate
				}
			}
			if row != nil {
				attempt, stop := context.WithTimeout(ctx, meshConnectTimeout)
				row.cancel = stop
				active++
				dev := row.event.Device
				go func() {
					device, err := probe(attempt, dev)
					stop()
					select {
					case results <- meshMetadataResult{row, device, err}:
					case <-ctx.Done():
					}
				}()
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				id := event.Device.ID
				if old := rows[id]; old != nil && old.cancel != nil {
					old.cancel()
				}
				delete(rows, id)
				if event.Kind == discovery.LANFound || event.Kind == discovery.LANUpdated {
					order++
					rows[id] = &meshMetadataRow{event: event, next: time.Now(), order: order, advertised: event.Probed}
				}
				if !send(event) {
					return
				}
			case result := <-results:
				active--
				row := rows[result.row.event.Device.ID]
				if row != result.row {
					break
				}
				row.cancel = nil
				row.next = time.Now().Add(refresh)
				row.event.Kind = discovery.LANUpdated
				row.event.Probed = result.err == nil
				row.event.ProbeFailed = result.err != nil
				if result.err == nil {
					row.event.Device = result.device
					// Agent/OS metadata changes infrequently. Leave bandwidth for
					// commands and for devices whose first probe has not completed.
					row.next = time.Now().Add(max(refresh, 5*time.Minute))
				} else {
					row.event.Device.ProbeFailure = result.device.ProbeFailure
				}
				if !send(row.event) {
					return
				}
			case <-tick.C:
			}
			schedule()
		}
	}()
	return out
}
