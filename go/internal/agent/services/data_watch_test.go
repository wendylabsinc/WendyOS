package services

import (
	"context"
	"errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }
func newFakeClock() *fakeClock           { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

type sentNotification struct {
	endpoint     string
	notification DetectionNotification
}

type endpointSender struct{ sent chan sentNotification }

func (s *endpointSender) Send(_ context.Context, endpoint string, n DetectionNotification) error {
	s.sent <- sentNotification{endpoint, n}
	return nil
}

func watchYAML(name, inference, extra string) string {
	return "kind: watch\nversion: 1\nname: " + name + "\non: {inference: " + inference + "}\n" + extra
}

func journal(t *testing.T, service *DataService, watches ...string) []data.CampaignNotification {
	t.Helper()
	items, _, _, err := service.manager.NotificationsFor("", "", "", true, watches)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestDocumentLeases(t *testing.T) {
	clock := newFakeClock()
	leases := documentLeases{now: clock.Now}
	if !leases.expired("w") {
		t.Fatal("a watch without a deadline must count as expired")
	}
	deadline := leases.extend("w", time.Minute)
	if !deadline.Equal(clock.Now().Add(time.Minute)) || leases.expired("w") {
		t.Fatalf("extend = %v", deadline)
	}
	clock.Add(30 * time.Second)
	renewed, ok := leases.renew("w", time.Minute)
	if !ok || !renewed.Equal(clock.Now().Add(time.Minute)) {
		t.Fatalf("renew = %v, %v", renewed, ok)
	}
	clock.Add(time.Minute)
	if !leases.expired("w") {
		t.Fatal("not expired at the deadline")
	}
	if _, ok := leases.renew("w", time.Minute); ok {
		t.Fatal("renew revived a lapsed lease")
	}
	if !leases.lapsed("w") || leases.lapsed("never-leased") {
		t.Fatal("lapsed must be true for a passed deadline and false for no deadline")
	}
	leases.extend("x", time.Minute)
	if leases.lapsed("x") {
		t.Fatal("a live lease counts as lapsed")
	}
	leases.forget("x")
	if _, ok := leases.deadline("x"); ok {
		t.Fatal("forget kept the deadline")
	}
}

func TestWatchDeliverWritesOneEntryPerWatch(t *testing.T) {
	service, factory, _ := newDocumentTestService(t)
	sender := &endpointSender{sent: make(chan sentNotification, 8)}
	startDocumentTestService(t, service, factory, sender)
	saveDocument(t, service, watchYAML("porch", "front-door", "notify:\n  - cloud\n  - webhook: https://example.com/hook\n"))
	saveDocument(t, service, watchYAML("chat-1", "front-door", "lease: 60s\n"))
	service.leases.extend("chat-1", time.Minute)
	saveDocument(t, service, watchYAML("elsewhere", "back-door", ""))

	base := inferenceNotification(data.InferenceDocument{}.Campaign(""), "v4l2:/dev/video0", nil)
	base.Inference, base.Event = "front-door", "front-door"
	service.watches.deliver("front-door", base)

	entries := journal(t, service, "porch", "chat-1", "elsewhere")
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	byWatch := map[string]data.CampaignNotification{}
	for _, entry := range entries {
		byWatch[entry.Watch] = entry
	}
	if byWatch["porch"].Leased || !byWatch["chat-1"].Leased || byWatch["porch"].ID == byWatch["chat-1"].ID {
		t.Fatalf("entries = %+v", byWatch)
	}
	if byWatch["porch"].Campaign != "" || byWatch["porch"].Inference != "front-door" {
		t.Fatalf("porch entry = %+v", byWatch["porch"])
	}
	endpoints := map[string]bool{}
	for i := 0; i < 2; i++ {
		sent := receiveInference(t, sender.sent)
		if sent.notification.Watch != "porch" {
			t.Fatalf("delivered for %q", sent.notification.Watch)
		}
		endpoints[sent.endpoint] = true
	}
	if !endpoints[""] || !endpoints["https://example.com/hook"] {
		t.Fatalf("deliveries went to %v; want Cloud and the webhook", endpoints)
	}
}

func TestLapsedLeaseWritesNothingBeforeRemoval(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	clock := newFakeClock()
	service.leases.now = clock.Now
	startDocumentTestService(t, service, factory, sender)
	saveDocument(t, service, watchYAML("chat-1", "front-door", "lease: 15s\n"))
	service.leases.extend("chat-1", 15*time.Second)
	clock.Add(16 * time.Second)
	base := DetectionNotification{Event: "front-door", Inference: "front-door", SourceID: "v4l2:/dev/video0", Count: 1, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano)}
	base.OccurredAt = time.Now().UTC().Format(time.RFC3339Nano)
	service.watches.deliver("front-door", base)
	if entries := journal(t, service, "chat-1"); len(entries) != 0 {
		t.Fatalf("a lapsed lease wrote %+v", entries)
	}
	if _, err := service.manager.Document(data.KindWatch, "chat-1"); err != nil {
		t.Fatal("the watch is gone before the expiry pass; this test needs it present")
	}
}

func TestExpireLeasesRemovesTheWatchAndWhatItOwns(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	clock := newFakeClock()
	service.leases.now = clock.Now
	startDocumentTestService(t, service, factory, sender)
	saveDocument(t, service, watchYAML("chat-1", "chat-1", "lease: 15s\n"))
	service.leases.extend("chat-1", 15*time.Second)
	saveDocument(t, service, strings.Replace(testInferenceDocumentYAML, "name: front-door", "name: chat-1\nowner: watch/chat-1", 1))
	session := receiveInference(t, factory.sessions)
	clock.Add(16 * time.Second)
	service.expireLeases()
	for _, ref := range [][2]string{{data.KindWatch, "chat-1"}, {data.KindInference, "chat-1"}} {
		if _, err := service.manager.Document(ref[0], ref[1]); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s/%s after expiry: %v", ref[0], ref[1], err)
		}
	}
	select {
	case <-session.closed:
	case <-time.After(8 * time.Second):
		t.Fatal("the owned inference kept running after its watch expired")
	}
}

func TestBootSweepRemovesLeasedWatchesWhatTheyOwnAndOrphans(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	saveDocument(t, service, watchYAML("chat-1", "chat-1", "lease: 60s\n"))
	saveDocument(t, service, strings.Replace(testInferenceDocumentYAML, "name: front-door", "name: chat-1\nowner: watch/chat-1", 1))
	saveDocument(t, service, watchYAML("porch", "front-door", ""))
	saveDocument(t, service, strings.Replace(testInferenceDocumentYAML, "name: front-door", "name: orphan\nowner: watch/gone", 1))
	startDocumentTestService(t, service, factory, sender)
	for _, ref := range [][2]string{{data.KindWatch, "chat-1"}, {data.KindInference, "chat-1"}, {data.KindInference, "orphan"}} {
		if _, err := service.manager.Document(ref[0], ref[1]); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s/%s survived the boot sweep: %v", ref[0], ref[1], err)
		}
	}
	if _, err := service.manager.Document(data.KindWatch, "porch"); err != nil {
		t.Fatalf("an unleased watch was swept: %v", err)
	}
	select {
	case <-factory.sessions:
		t.Fatal("an inference started for a swept document")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestExpireLeasesSkipsAWatchRenewedWhileWaitingForTheLock(t *testing.T) {
	service, factory, sender := newDocumentTestService(t)
	clock := newFakeClock()
	service.leases.now = clock.Now
	startDocumentTestService(t, service, factory, sender)
	saveDocument(t, service, watchYAML("chat-1", "chat-1", "lease: 15s\n"))
	service.leases.extend("chat-1", 15*time.Second)
	clock.Add(16 * time.Second)
	service.deploymentMu.Lock()
	done := make(chan struct{})
	go func() { service.expireLeases(); close(done) }()
	time.Sleep(100 * time.Millisecond)
	service.leases.extend("chat-1", 15*time.Second)
	service.deploymentMu.Unlock()
	receiveInference(t, done)
	if _, err := service.manager.Document(data.KindWatch, "chat-1"); err != nil {
		t.Fatalf("a renewed watch was removed: %v", err)
	}
	if _, ok := service.leases.deadline("chat-1"); !ok {
		t.Fatal("the renewed deadline was forgotten")
	}
}

type cloudFailsSender struct{}

func (cloudFailsSender) Send(_ context.Context, endpoint string, _ DetectionNotification) error {
	if endpoint == "" {
		return status.Error(codes.PermissionDenied, "watch not registered")
	}
	return nil
}

func TestDeliveryErrorKeepsTheCloudFailureAfterAWebhookSuccess(t *testing.T) {
	service, factory, _ := newDocumentTestService(t)
	startDocumentTestService(t, service, factory, cloudFailsSender{})
	saveDocument(t, service, watchYAML("porch", "front-door", "notify:\n  - cloud\n  - webhook: https://example.com/hook\n"))
	base := DetectionNotification{Event: "front-door", Inference: "front-door", SourceID: "v4l2:/dev/video0", Count: 1, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano)}
	service.watches.deliver("front-door", base)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := service.watches.deliveryError("porch"); strings.Contains(got, "cloud: ") {
			if strings.Contains(got, "webhook") {
				t.Fatalf("webhook succeeded but is reported: %q", got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("deliveryError = %q; want the Cloud failure", service.watches.deliveryError("porch"))
}

// blockingWebhookSender holds every webhook delivery until release closes or
// the delivery's context ends; Cloud deliveries go through.
type blockingWebhookSender struct {
	release chan struct{}
	sent    chan sentNotification
}

func (s *blockingWebhookSender) Send(ctx context.Context, endpoint string, n DetectionNotification) error {
	if endpoint != "" {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	s.sent <- sentNotification{endpoint, n}
	return nil
}

func TestSlowWebhookDoesNotDelayAnotherWatchsCloudDelivery(t *testing.T) {
	service, factory, _ := newDocumentTestService(t)
	sender := &blockingWebhookSender{release: make(chan struct{}), sent: make(chan sentNotification, 8)}
	t.Cleanup(func() { close(sender.release) })
	startDocumentTestService(t, service, factory, sender)
	saveDocument(t, service, watchYAML("slow", "front-door", "notify:\n  - webhook: https://example.com/hook\n"))
	saveDocument(t, service, watchYAML("fast", "front-door", "notify:\n  - cloud\n"))
	base := DetectionNotification{Event: "front-door", Inference: "front-door", SourceID: "v4l2:/dev/video0", Count: 1, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano)}
	// More arrivals than one queue holds, so the slow watch's queue fills.
	for i := 0; i < watchQueueSize+4; i++ {
		service.watches.deliver("front-door", base)
	}
	for i := 0; i < watchQueueSize+4; i++ {
		if got := receiveInference(t, sender.sent); got.notification.Watch != "fast" {
			t.Fatalf("delivered for %q", got.notification.Watch)
		}
	}
	if got := service.watches.deliveryError("slow"); !strings.Contains(got, "queue full") {
		t.Fatalf("slow watch's error = %q; want its own queue-full error", got)
	}
	if got := service.watches.deliveryError("fast"); got != "" {
		t.Fatalf("fast watch's error = %q", got)
	}
}

func TestRemovedWatchForgetsItsDeliveryError(t *testing.T) {
	service, factory, _ := newDocumentTestService(t)
	startDocumentTestService(t, service, factory, cloudFailsSender{})
	const document = "notify:\n  - cloud\n"
	saveDocument(t, service, watchYAML("porch", "front-door", document))
	base := DetectionNotification{Event: "front-door", Inference: "front-door", SourceID: "v4l2:/dev/video0", Count: 1, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano)}
	service.watches.deliver("front-door", base)
	eventually(t, "the Cloud failure", func() bool { return service.watches.deliveryError("porch") != "" })
	if _, err := service.removeDocumentTree(data.KindWatch, "porch"); err != nil {
		t.Fatal(err)
	}
	saveDocument(t, service, watchYAML("porch", "front-door", document))
	if got := service.watches.deliveryError("porch"); got != "" {
		t.Fatalf("re-applied watch reports %q", got)
	}
}
