package backend

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	items, err := inspectSavedEpisodes(ctx, client, 0, func(e *pb.DataEpisode, m *data.Manifest) *CampaignClip {
		if m == nil || m.Trigger.CampaignName == "" {
			return nil
		}
		return &CampaignClip{ID: m.ID, Campaign: m.Trigger.CampaignName, State: m.State, Reason: m.Trigger.Reason, Started: m.StartedUnixNanos, Size: e.GetSizeBytes(), Duration: savedDuration(m)}
	})
	if err != nil {
		return CampaignClips{}, err
	}
	out := CampaignClips{}
	missing := 0
	for _, item := range items {
		if item.missing {
			missing++
		}
		if item.value != nil {
			out.Items = append(out.Items, *item.value)
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
