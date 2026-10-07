package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// watchDataClient is a small in-memory device: campaigns, their inference
// state, a notification journal and camera sources. calls records the order of
// device operations so tests can check, for example, journal-before-deploy.
type watchDataClient struct {
	agentpbv2.DataServiceClient
	mu           sync.Mutex
	calls        []string
	deployed     map[string]data.Campaign
	deployedYAML map[string][]byte               // the raw plan each campaign was sent as
	inference    map[string]data.InferenceStatus // default: running
	renews       map[string]int
	removed      []string
	deployErr    error
	sourcesErr   error
	renewErr     error
	inspectErr   error
	removeErr    error
	removeWait   time.Duration
	deployWait   time.Duration // CampaignDeploy sleeps this long before doing anything else
	journal      []data.CampaignNotification
	gapOnce      bool
	oldJournal   bool // Events does not mark the response as the notification journal
	sources      []*agentpbv2.DataSource
}

func newWatchDataClient() *watchDataClient {
	return &watchDataClient{
		deployed: map[string]data.Campaign{}, deployedYAML: map[string][]byte{}, inference: map[string]data.InferenceStatus{}, renews: map[string]int{},
		sources: []*agentpbv2.DataSource{
			{Id: "v4l2:/dev/video0", Kind: "camera", Healthy: true, Detail: "Brio 101 VIDEO_TRANSPORT_USB"},
			{Id: "v4l2:/dev/video2", Kind: "camera", Healthy: false, Detail: "Unplugged VIDEO_TRANSPORT_USB"},
			{Id: "audio:55", Kind: "audio", Healthy: true, Detail: "Built-in Audio"},
		},
	}
}

func (f *watchDataClient) record(call string) { f.calls = append(f.calls, call) }

func (f *watchDataClient) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *watchDataClient) CampaignDeploy(_ context.Context, r *agentpbv2.DataCampaignDeployRequest, _ ...grpc.CallOption) (*agentpbv2.DataCampaign, error) {
	f.mu.Lock()
	wait := f.deployWait
	f.mu.Unlock()
	time.Sleep(wait)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deployErr != nil {
		f.record("deploy-error")
		return nil, f.deployErr
	}
	campaign, err := data.ParseCampaign(r.GetCampaignYaml())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	f.record("deploy:" + campaign.Name)
	f.deployed[campaign.Name] = campaign
	f.deployedYAML[campaign.Name] = append([]byte(nil), r.GetCampaignYaml()...)
	return &agentpbv2.DataCampaign{Name: campaign.Name, State: campaign.State, Revision: campaign.Revision}, nil
}

func (f *watchDataClient) CampaignInspect(_ context.Context, r *agentpbv2.DataCampaignInspectRequest, _ ...grpc.CallOption) (*agentpbv2.DataCampaign, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return nil, f.inspectErr
	}
	campaign, ok := f.deployed[r.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "campaign not found")
	}
	inference, ok := f.inference[r.GetName()]
	if !ok {
		inference = data.InferenceStatus{State: "running"}
	}
	campaign.InferenceStatus = &inference
	plan, _ := json.Marshal(campaign)
	return &agentpbv2.DataCampaign{Name: campaign.Name, State: campaign.State, PlanJson: plan}, nil
}

func (f *watchDataClient) CampaignRenew(_ context.Context, r *agentpbv2.DataCampaignRenewRequest, _ ...grpc.CallOption) (*agentpbv2.DataCampaignRenewResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.renewErr != nil {
		return nil, f.renewErr
	}
	if _, ok := f.deployed[r.GetName()]; !ok {
		return nil, status.Error(codes.NotFound, "campaign not found")
	}
	f.renews[r.GetName()]++
	return &agentpbv2.DataCampaignRenewResponse{ExpiresUnixNanos: time.Now().Add(time.Minute).UnixNano()}, nil
}

func (f *watchDataClient) CampaignRemove(ctx context.Context, r *agentpbv2.DataCampaignRemoveRequest, _ ...grpc.CallOption) (*agentpbv2.DataCampaignRemoveResponse, error) {
	f.mu.Lock()
	wait := f.removeWait
	f.mu.Unlock()
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("remove:" + r.GetName())
	f.removed = append(f.removed, r.GetName())
	if f.removeErr != nil {
		return nil, f.removeErr
	}
	if _, ok := f.deployed[r.GetName()]; !ok {
		return nil, status.Error(codes.NotFound, "campaign not found")
	}
	delete(f.deployed, r.GetName())
	return &agentpbv2.DataCampaignRemoveResponse{}, nil
}

// Events serves the notification journal like the agent: an empty cursor
// without replay returns only the tail; a cursor returns entries after it.
func (f *watchDataClient) Events(_ context.Context, r *agentpbv2.DataEventsRequest, _ ...grpc.CallOption) (*agentpbv2.DataEventsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("events:" + r.GetCursor())
	var after uint64
	if _, number, ok := strings.Cut(r.GetCursor(), ":"); ok {
		after, _ = strconv.ParseUint(number, 10, 64)
	}
	out := []data.CampaignNotification{}
	var tail uint64
	for _, entry := range f.journal {
		tail = max(tail, entry.Sequence)
		if r.GetCursor() != "" && entry.Sequence > after {
			out = append(out, entry)
		}
	}
	raw, _ := json.Marshal(out)
	gap := f.gapOnce
	f.gapOnce = false
	return &agentpbv2.DataEventsResponse{EventsJson: raw, Cursor: fmt.Sprintf("epoch:%d", tail), Gap: gap, Notifications: !f.oldJournal}, nil
}

func (f *watchDataClient) Sources(context.Context, *agentpbv2.DataSourcesRequest, ...grpc.CallOption) (*agentpbv2.DataSourcesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sourcesErr != nil {
		return nil, f.sourcesErr
	}
	return &agentpbv2.DataSourcesResponse{Sources: f.sources}, nil
}

// publish appends a detection notification for campaign to the journal.
func (f *watchDataClient) publish(campaign string, detections ...data.NotificationDetection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sequence := uint64(len(f.journal) + 1)
	f.journal = append(f.journal, data.CampaignNotification{
		ID: strconv.FormatUint(sequence, 10), Event: campaign + ".detected", Campaign: campaign, Count: len(detections),
		Detections: detections, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano), Sequence: sequence,
	})
}

func (f *watchDataClient) setInference(campaign string, state data.InferenceStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inference[campaign] = state
}

func (f *watchDataClient) campaign(name string) (data.Campaign, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.deployed[name]
	return c, ok
}

func (f *watchDataClient) deployedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.deployed))
	for name := range f.deployed {
		names = append(names, name)
	}
	return names
}
