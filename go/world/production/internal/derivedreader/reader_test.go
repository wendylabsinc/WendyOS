package derivedreader

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc"
)

type budgetClient struct {
	appv1.WorldAppServiceClient
	config   Config
	physical time.Time
	queries  int
}

func (c *budgetClient) Clock(_ context.Context, q *appv1.ClockRequest, _ ...grpc.CallOption) (*appv1.ClockReply, error) {
	return &appv1.ClockReply{Nonce: bytes.Clone(q.Nonce), Physical: &appv1.PhysicalClock{EarliestUnixNs: c.physical.UnixNano(), LatestUnixNs: c.physical.Add(time.Millisecond).UnixNano(), BootId: "budget-clock", MonotonicNs: int64(time.Second), Source: "test"}, Tenant: c.config.Tenant, Site: c.config.Site, Device: c.config.Device, App: c.config.App, Service: c.config.Service, ExecutionDomain: "simulation", DeploymentGeneration: 1, ServerBootId: strings.Repeat("c", 32), SampleClockDomain: "simulation:" + c.config.RunID, SampleUnixNs: time.Unix(1700000000, 0).UnixNano()}, nil
}
func (c *budgetClient) Query(context.Context, *appv1.QueryRequest, ...grpc.CallOption) (*appv1.QueryReply, error) {
	c.queries++
	return &appv1.QueryReply{}, nil
}
func TestStartupTimeBudgetRefusesBeforeQuery(t *testing.T) {
	physical := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	id := testidentity.NewWith(t, physical, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/22222222-2222-4222-8222-222222222222")
	c := Config{Version: 1, Tenant: id.Tenant, Site: "office", Device: id.Principal, App: "com.example.reader", Service: "reader", RootsDER: [][]byte{id.RootDER}, RunID: "budget", Frame: world.FrameRef{ID: "floor", Revision: "v1"}, Source: Source{Device: id.Principal, App: "com.example.source", Service: "source", Stream: "results"}, DurationMS: 10000}
	api := &budgetClient{config: c, physical: physical}
	s, err := New(c, api, func(serviceorigin.PublishRequest) (Interpretation[string], error) {
		t.Fatal("decoder called without a result")
		return Interpretation[string]{}, ErrRefused
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.startupBegan = time.Now().Add(-startupBudget)
	r, err := s.Step(context.Background())
	if err == nil || r.Status != "failed" || api.queries != 0 {
		t.Fatal("expired startup budget issued Query")
	}
	if _, err = s.Step(context.Background()); err == nil {
		t.Fatal("terminal budget failure reopened")
	}
}
