package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type CampaignClip struct {
	ID, Campaign, State, Reason string
	Started, Size               int64
	Duration                    float64 // Seconds; -1 while recording or when unavailable.
}
type CampaignClips struct {
	Items   []CampaignClip
	Warning string
}

func (b *Backend) CampaignClips(session uint64) (CampaignClips, error) {
	b.mu.Lock()
	if session == 0 {
		b.mu.Unlock()
		return CampaignClips{}, errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		b.mu.Unlock()
		return CampaignClips{}, err
	}
	life := b.life
	if life == nil {
		life = context.Background()
	}
	ctx, cancel := context.WithTimeout(life, time.Minute)
	client := conn.DataService
	b.mu.Unlock()
	defer cancel()
	return readCampaignClips(ctx, client)
}

// Inspect each manifest once for all campaigns. Display names are not campaign
// identities. Only the recorded trigger association establishes ownership.
func readCampaignClips(ctx context.Context, client pb.DataServiceClient) (CampaignClips, error) {
	list, err := client.Episodes(ctx, &pb.DataEpisodesRequest{})
	if err != nil {
		return CampaignClips{}, err
	}
	episodes := list.GetEpisodes()
	rows := make([]*CampaignClip, len(episodes))
	failed := make([]bool, len(episodes))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for n := 0; n < 4; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				e := episodes[i]
				r, err := client.Inspect(ctx, &pb.DataInspectRequest{Episode: e.GetId()})
				var m data.Manifest
				if err != nil || len(r.GetManifestJson()) > visualFileLimit || json.Unmarshal(r.GetManifestJson(), &m) != nil || m.ID != e.GetId() {
					failed[i] = true
					continue
				}
				if m.Trigger.CampaignName == "" {
					continue
				}
				duration := -1.0
				start, end := m.StartedEpisodeNS, m.StoppedEpisodeNS
				for _, source := range m.Sources {
					if source.Count > 0 {
						start = min(start, source.ActualOffset)
					}
				}
				if m.CaptureStoppedEpisodeNS != 0 {
					end = m.CaptureStoppedEpisodeNS
				}
				if (m.State == "complete" || m.State == "interrupted") && end >= start {
					duration = (float64(end) - float64(start)) / 1e9
				}
				rows[i] = &CampaignClip{ID: m.ID, Campaign: m.Trigger.CampaignName, State: m.State, Reason: m.Trigger.Reason, Started: m.StartedUnixNanos, Size: e.GetSizeBytes(), Duration: duration}
			}
		}()
	}
	for i := range episodes {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return CampaignClips{}, err
	}
	out := CampaignClips{}
	missing := 0
	for i, row := range rows {
		if failed[i] {
			missing++
		}
		if row != nil {
			out.Items = append(out.Items, *row)
		}
	}
	if missing > 0 {
		out.Warning = fmt.Sprintf("Could not read %d recordings. Clip counts may be incomplete. Refresh to try again.", missing)
	}
	sort.Slice(out.Items, func(i, j int) bool {
		if out.Items[i].Started == out.Items[j].Started {
			return out.Items[i].ID < out.Items[j].ID
		}
		return out.Items[i].Started > out.Items[j].Started
	})
	return out, nil
}
