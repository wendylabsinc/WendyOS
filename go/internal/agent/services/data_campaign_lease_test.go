package services

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type leaseTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *leaseTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *leaseTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// leaseTestVideo hands out one-frame subscriptions and reports each close, so
// a test can see a camera being released.
type leaseTestVideo struct{ closed chan string }

func (v *leaseTestVideo) SubscribeSensor(_ context.Context, id string) (sensorSubscription, error) {
	return &leaseTestSubscription{id: id, closed: v.closed}, nil
}

type leaseTestSubscription struct {
	inferenceTestSubscription
	id     string
	closed chan string
	once   sync.Once
}

func (s *leaseTestSubscription) Close() { s.once.Do(func() { s.closed <- s.id }) }

func leasedTestYAML(name, lease string) []byte {
	return []byte(`version: 1
name: ` + name + `
lease: ` + lease + `
sources:
  - camera: v4l2:/dev/video0
inference:
  model: PekingU/rtdetr_r18vd
  revision: ac77a11ff0170a41b771c03264987f8ce2b0d753
  labels: [person]
  threshold: 0.5
  rate: 2
  event: ` + name + `.detected
  clear_after: 5s
  cooldown: 30s
notify:
  on: detection
`)
}

type leaseTest struct {
	service *DataService
	clock   *leaseTestClock
	factory *inferenceTestFactory
	sender  *inferenceTestSender
	video   *leaseTestVideo
}

// newLeaseTest starts a data service with one camera, a fake lease clock and
// fake model runtime. before, if set, runs against the manager before
// inference starts, as an earlier agent run would have.
func newLeaseTest(t *testing.T, before func(*data.Manager)) *leaseTest {
	t.Helper()
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if before != nil {
		before(manager)
	}
	lt := &leaseTest{
		service: NewDataService(manager),
		clock:   &leaseTestClock{now: time.Unix(1_800_000_000, 0)},
		factory: &inferenceTestFactory{sessions: make(chan *inferenceTestSession, 8)},
		sender:  &inferenceTestSender{requests: make(chan DetectionNotification, 8)},
		video:   &leaseTestVideo{closed: make(chan string, 32)},
	}
	lt.service.addAdapter(&inferenceTestAdapter{sources: []data.Source{{ID: "v4l2:/dev/video0", Kind: "camera", Healthy: true}}})
	lt.service.video = lt.video
	lt.service.leases.now = lt.clock.Now
	stop := lt.service.StartCampaignInference(context.Background(), lt.factory, lt.sender)
	t.Cleanup(func() {
		stop()
		for _, key := range manager.ActiveEpisodeKeys() {
			_, _ = lt.service.stopCapture(context.Background(), key)
		}
	})
	return lt
}

func deployLeased(t *testing.T, service *DataService, name, lease string) {
	t.Helper()
	if _, err := service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: leasedTestYAML(name, lease)}); err != nil {
		t.Fatal(err)
	}
}

func renew(service *DataService, name string) (*agentpbv2.DataCampaignRenewResponse, error) {
	return service.CampaignRenew(context.Background(), &agentpbv2.DataCampaignRenewRequest{Name: name})
}

func TestLeasedDeployStartsLeaseAndRenewExtendsIt(t *testing.T) {
	lt := newLeaseTest(t, nil)
	start := lt.clock.Now()
	deployLeased(t, lt.service, "chat-1", "15s")
	lt.clock.Advance(10 * time.Second)
	renewed, err := renew(lt.service, "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := start.Add(25 * time.Second).UnixNano(); renewed.GetExpiresUnixNanos() != want {
		t.Fatalf("expires %d, want now + lease = %d", renewed.GetExpiresUnixNanos(), want)
	}
}

func TestRenewAfterDeadlineIsNotFound(t *testing.T) {
	lt := newLeaseTest(t, nil)
	deployLeased(t, lt.service, "chat-1", "15s")
	lt.clock.Advance(15 * time.Second)
	if _, err := renew(lt.service, "chat-1"); status.Code(err) != codes.NotFound {
		t.Fatalf("renewing a lapsed lease: %v, want NotFound", err)
	}
}

func TestRedeployRestartsLease(t *testing.T) {
	lt := newLeaseTest(t, nil)
	start := lt.clock.Now()
	deployLeased(t, lt.service, "chat-1", "15s")
	lt.clock.Advance(10 * time.Second)
	deployLeased(t, lt.service, "chat-1", "15s")
	lt.clock.Advance(10 * time.Second)
	renewed, err := renew(lt.service, "chat-1")
	if err != nil {
		t.Fatalf("the redeploy did not restart the lease: %v", err)
	}
	if want := start.Add(35 * time.Second).UnixNano(); renewed.GetExpiresUnixNanos() != want {
		t.Fatalf("expires %d, want %d", renewed.GetExpiresUnixNanos(), want)
	}
}

func TestRenewRejectsOrdinaryAndUnknownCampaigns(t *testing.T) {
	lt := newLeaseTest(t, nil)
	if _, err := lt.service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: inferenceTestYAML(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := renew(lt.service, "people-all-cameras"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("renewing an ordinary campaign: %v, want FailedPrecondition", err)
	}
	if _, err := renew(lt.service, "missing"); status.Code(err) != codes.NotFound {
		t.Fatalf("renewing a missing campaign: %v, want NotFound", err)
	}
	if _, err := renew(lt.service, "../escape"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("renewing a malformed name: %v, want InvalidArgument", err)
	}
}

func TestLeaseCannotBeAddedOrRemovedByRedeploy(t *testing.T) {
	lt := newLeaseTest(t, nil)
	deployLeased(t, lt.service, "chat-1", "15s")
	ordinary := strings.Replace(string(inferenceTestYAML(t)), "name: people-all-cameras", "name: chat-1", 1)
	if _, err := lt.service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: []byte(ordinary)}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("dropping a lease by redeploying: %v, want FailedPrecondition", err)
	}
	if campaign, err := lt.service.manager.Campaign("chat-1"); err != nil || !campaign.Leased() {
		t.Fatalf("the refused redeploy changed the plan: %+v %v", campaign, err)
	}
	if _, err := lt.service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: inferenceTestYAML(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := lt.service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: leasedTestYAML("people-all-cameras", "15s")}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("adding a lease by redeploying: %v, want FailedPrecondition", err)
	}
}
