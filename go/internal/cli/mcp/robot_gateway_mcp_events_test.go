package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

func gatewayMCPEventsFixture(t *testing.T) (*RobotGateway, context.Context) {
	t.Helper()
	cfg := gatewayTestConfig()
	cfg.StateDirectory = t.TempDir()
	g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return nil, fmt.Errorf("unexpected device connection")
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.mcpEvents.close)
	ctx := context.WithValue(context.Background(), gatewayPrincipalKey{}, gatewayPrincipal{Subject: "alice", Scopes: robotGatewayScopes})
	ctx = context.WithValue(ctx, gatewayLocalContextKey{}, true)
	g.mcpEvents.read = func(context.Context, string, json.RawMessage, string, bool) (gatewayMCPEventBatch, error) {
		return gatewayMCPEventBatch{Cursor: "epoch:0"}, nil
	}
	return g, ctx
}

func gatewayMCPEventsSecret(letter string) string {
	return "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat(letter, 32)))
}

func gatewayMCPSubscribeRequest(secret, args string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"name":"wendy.data.notification","arguments":%s,"delivery":{"mode":"webhook","url":"https://receiver.example/events","secret":%q}}`, args, secret))
}

func gatewayMCPEventTestReceiver(verification *int, event func(*http.Request, []byte) int) *http.Client {
	return &http.Client{Transport: gatewayMCPRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		var payload map[string]any
		json.Unmarshal(body, &payload)
		if payload["type"] == "verification" {
			*verification++
			result, _ := json.Marshal(map[string]any{"challenge": payload["challenge"]})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(result)))}, nil
		}
		status := event(request, body)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
}

func TestMCPEventSubscriptionCanonicalIdentityTTLRotationAndOwnership(t *testing.T) {
	g, ctx := gatewayMCPEventsFixture(t)
	verifications := 0
	g.mcpEvents.client = gatewayMCPEventTestReceiver(&verifications, func(*http.Request, []byte) int { return 204 })
	secret := gatewayMCPEventsSecret("a")
	result, err := g.handleMCPEventRequest(ctx, "events/subscribe", gatewayMCPSubscribeRequest(secret, `{"robot_id":"alpha","campaign":"people"}`))
	if err != nil {
		t.Fatal(err)
	}
	id := result.(map[string]any)["id"].(string)
	result, err = g.handleMCPEventRequest(ctx, "events/subscribe", gatewayMCPSubscribeRequest(secret, `{"campaign":"people","robot_id":"alpha"}`))
	if err != nil || result.(map[string]any)["id"] != id || verifications != 1 || len(g.mcpEvents.entries) != 1 {
		t.Fatalf("not idempotent result=%v err=%v verifications=%d", result, err, verifications)
	}
	rotated := gatewayMCPEventsSecret("b")
	request := strings.TrimSuffix(string(gatewayMCPSubscribeRequest(rotated, `{"robot_id":"alpha","campaign":"people"}`)), "}") + `,"ttlMs":1000}`
	before := time.Now()
	if _, err = g.handleMCPEventRequest(ctx, "events/subscribe", []byte(request)); err != nil {
		t.Fatal(err)
	}
	entry := g.mcpEvents.entries[id]
	if entry.state.Secret != rotated || entry.state.OldSecret != secret || entry.state.Expires.Sub(before) > time.Second+100*time.Millisecond || verifications != 2 {
		t.Fatal("rotation or requested TTL was not applied")
	}
	info, fsErr := os.Stat(filepath.Join(g.mcpEvents.path, id+".json"))
	if fsErr != nil || info.Mode().Perm() != 0600 {
		t.Fatal("subscription secret file is not private", fsErr)
	}
	bob := context.WithValue(ctx, gatewayPrincipalKey{}, gatewayPrincipal{Subject: "bob", Scopes: robotGatewayScopes})
	if _, err = g.handleMCPEventRequest(bob, "events/unsubscribe", gatewayMCPSubscribeRequest(secret, `{"robot_id":"alpha","campaign":"people"}`)); err == nil {
		t.Fatal("another principal stopped subscription")
	}
	if len(g.mcpEvents.entries) != 1 {
		t.Fatal("other principal changed subscription")
	}
	for i := 0; i < 2; i++ {
		if _, err = g.handleMCPEventRequest(ctx, "events/unsubscribe", gatewayMCPSubscribeRequest(secret, `{"robot_id":"alpha","campaign":"people"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if len(g.mcpEvents.entries) != 0 {
		t.Fatal("unsubscribe retained active subscription")
	}
}

func TestMCPEventOutboxRetriesPersistAndNeverSkipPendingCursor(t *testing.T) {
	g, ctx := gatewayMCPEventsFixture(t)
	now := time.Now()
	g.mcpEvents.now = func() time.Time { return now }
	verifications, deliveries := 0, 0
	var firstBody []byte
	client := gatewayMCPEventTestReceiver(&verifications, func(request *http.Request, body []byte) int {
		deliveries++
		if deliveries == 1 {
			firstBody = append([]byte(nil), body...)
			return 503
		}
		if deliveries == 2 && string(firstBody) != string(body) {
			t.Fatal("retry changed serialized event or ID")
		}
		return 204
	})
	g.mcpEvents.client = client
	result, rpcErr := g.handleMCPEventRequest(ctx, "events/subscribe", gatewayMCPSubscribeRequest(gatewayMCPEventsSecret("a"), `{"robot_id":"alpha"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	id := result.(map[string]any)["id"].(string)
	readCalls := 0
	g.mcpEvents.read = func(_ context.Context, _ string, _ json.RawMessage, cursor string, _ bool) (gatewayMCPEventBatch, error) {
		readCalls++
		if cursor != "epoch:0" {
			t.Fatal("read skipped pending notifications", cursor)
		}
		return gatewayMCPEventBatch{Cursor: "epoch:3", Events: []gatewayMCPEventOccurrence{{ID: "n1", Timestamp: now.UTC().Format(time.RFC3339Nano), Data: map[string]any{"event": "person"}, Cursor: "epoch:1"}, {ID: "n2", Timestamp: now.UTC().Format(time.RFC3339Nano), Data: map[string]any{"event": "person"}, Cursor: "epoch:2"}}}, nil
	}
	g.mcpEvents.step(ctx, g.mcpEvents.entries[id])
	if deliveries != 1 || g.mcpEvents.entries[id].state.Cursor != "epoch:0" {
		t.Fatal("failed delivery advanced cursor")
	}
	restarted, err := NewRobotGateway(g.cfg, g.connect)
	if err != nil {
		t.Fatal(err)
	}
	restarted.mcpEvents.client = client
	restarted.mcpEvents.now = func() time.Time { return now }
	restarted.mcpEvents.read = func(context.Context, string, json.RawMessage, string, bool) (gatewayMCPEventBatch, error) {
		t.Fatal("re-read before durable outbox drained")
		return gatewayMCPEventBatch{}, nil
	}
	entry := restarted.mcpEvents.entries[id]
	if len(entry.state.Pending) != 2 || entry.state.Cursor != "epoch:0" {
		t.Fatal("restart lost outbox")
	}
	now = now.Add(3 * time.Second)
	restarted.mcpEvents.step(ctx, entry)
	if deliveries != 2 || entry.state.Cursor != "epoch:1" || len(entry.state.Pending) != 1 {
		t.Fatal("retry acknowledgement skipped second pending notification")
	}
	restarted.mcpEvents.step(ctx, entry)
	if deliveries != 3 || entry.state.Cursor != "epoch:3" || len(entry.state.Pending) != 0 || readCalls != 1 {
		t.Fatal("outbox did not advance to source tail after final ack")
	}
}

func TestMCPEventRevocationExpirationAndPermanentFailureStopDelivery(t *testing.T) {
	for _, mode := range []string{"revoked", "expired", "gone", "too_large", "retry_limit"} {
		t.Run(mode, func(t *testing.T) {
			g, ctx := gatewayMCPEventsFixture(t)
			now := time.Now()
			g.mcpEvents.now = func() time.Time { return now }
			verifications, deliveries := 0, 0
			g.mcpEvents.client = gatewayMCPEventTestReceiver(&verifications, func(*http.Request, []byte) int {
				deliveries++
				switch mode {
				case "gone":
					return 410
				case "too_large":
					return 413
				default:
					return 503
				}
			})
			result, rpcErr := g.handleMCPEventRequest(ctx, "events/subscribe", gatewayMCPSubscribeRequest(gatewayMCPEventsSecret("a"), `{"robot_id":"alpha"}`))
			if rpcErr != nil {
				t.Fatal(rpcErr)
			}
			entry := g.mcpEvents.entries[result.(map[string]any)["id"].(string)]
			g.mcpEvents.enqueue(&entry.state, gatewayMCPEventBatch{Cursor: "epoch:1", Events: []gatewayMCPEventOccurrence{{ID: "n1", Cursor: "epoch:1", Timestamp: now.UTC().Format(time.RFC3339Nano), Data: map[string]any{}}}})
			if mode == "revoked" {
				g.cfg.Grants[0].Scopes = []string{RobotReadScope}
			}
			if mode == "expired" {
				entry.state.Expires = now
			}
			for i := 0; i < 10; i++ {
				g.mcpEvents.step(ctx, entry)
				now = now.Add(300 * time.Second)
			}
			expected := 1
			if mode == "revoked" || mode == "expired" {
				expected = 0
			}
			if mode == "retry_limit" {
				expected = 8
			}
			if deliveries != expected || entry.state.Cursor != "epoch:0" {
				t.Fatalf("mode=%s deliveries=%d cursor=%s", mode, deliveries, entry.state.Cursor)
			}
		})
	}
}

func TestMCPEventHTTPAccountRecheckedBeforeDelivery(t *testing.T) {
	g, ctx := gatewayMCPEventsFixture(t)
	ctx = context.WithValue(ctx, gatewayLocalContextKey{}, false)
	ctx = context.WithValue(ctx, gatewayEventCredentialKey{}, "token")
	verifications, deliveries, authChecks := 0, 0, 0
	g.mcpEvents.client = gatewayMCPEventTestReceiver(&verifications, func(*http.Request, []byte) int { deliveries++; return 204 })
	g.eventAuthenticate = func(context.Context, string) (gatewayPrincipal, error) {
		authChecks++
		return gatewayPrincipal{}, fmt.Errorf("revoked")
	}
	result, rpcErr := g.handleMCPEventRequest(ctx, "events/subscribe", gatewayMCPSubscribeRequest(gatewayMCPEventsSecret("a"), `{"robot_id":"alpha"}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	entry := g.mcpEvents.entries[result.(map[string]any)["id"].(string)]
	g.mcpEvents.step(context.Background(), entry)
	if authChecks != 1 || deliveries != 0 {
		t.Fatal("revoked HTTP account was not rechecked")
	}
}

type gatewayMCPNotificationSource struct {
	agentpbv2.DataServiceClient
	response *agentpbv2.DataEventsResponse
	request  *agentpbv2.DataEventsRequest
}

func (source *gatewayMCPNotificationSource) Events(_ context.Context, request *agentpbv2.DataEventsRequest, _ ...grpc.CallOption) (*agentpbv2.DataEventsResponse, error) {
	source.request = request
	return source.response, nil
}

func TestMCPEventSourceRequiresNotificationJournalAndPreservesFacts(t *testing.T) {
	g, ctx := gatewayMCPEventsFixture(t)
	notification := data.CampaignNotification{ID: "notification-id", Event: "person_detected", Campaign: "people", SourceID: "camera", Count: 2, Sequence: 3, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano)}
	raw, _ := json.Marshal([]data.CampaignNotification{notification})
	source := &gatewayMCPNotificationSource{response: &agentpbv2.DataEventsResponse{EventsJson: raw, Cursor: "epoch:4", Notifications: true}}
	g.connect = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{DataService: source}, nil
	}
	args := json.RawMessage(`{"robot_id":"alpha","campaign":"people","event":"person_detected"}`)
	batch, err := g.readMCPEventSource(ctx, gatewayMCPNotificationEvent, args, "epoch:1", true)
	if err != nil || len(batch.Events) != 1 || batch.Events[0].ID != notification.ID || batch.Events[0].Timestamp != notification.OccurredAt || batch.Events[0].Cursor != "epoch:3" {
		t.Fatalf("notification facts not preserved: %+v %v", batch, err)
	}
	if !source.request.NotificationsOnly || source.request.AppId != "sh.wendy.campaign.people" || source.request.Event != "person_detected" || source.request.Cursor != "epoch:1" {
		t.Fatal("source filter omitted", source.request)
	}
	source.response.Notifications = false
	if _, err = g.readMCPEventSource(ctx, gatewayMCPNotificationEvent, args, "", false); err == nil || !strings.Contains(err.Error(), "update") {
		t.Fatal("old agent's raw events were accepted as notifications", err)
	}
}

func TestMCPEventExpiredRefreshReplaysWithoutSkippingOutbox(t *testing.T) {
	g, ctx := gatewayMCPEventsFixture(t)
	verifications := 0
	g.mcpEvents.client = gatewayMCPEventTestReceiver(&verifications, func(*http.Request, []byte) int { return 204 })
	request := gatewayMCPSubscribeRequest(gatewayMCPEventsSecret("a"), `{"robot_id":"alpha"}`)
	result, rpcErr := g.handleMCPEventRequest(ctx, "events/subscribe", request)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	entry := g.mcpEvents.entries[result.(map[string]any)["id"].(string)]
	entry.state.Expires = time.Now().Add(-time.Second)
	reads := 0
	g.mcpEvents.read = func(_ context.Context, _ string, _ json.RawMessage, cursor string, replay bool) (gatewayMCPEventBatch, error) {
		reads++
		if cursor != "epoch:2" || !replay {
			t.Fatal("expired refresh ignored replay cursor")
		}
		return gatewayMCPEventBatch{Cursor: "epoch:5", Truncated: true, Events: []gatewayMCPEventOccurrence{{ID: "n5", Cursor: "epoch:5", Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Data: map[string]any{}}}}, nil
	}
	request = []byte(strings.TrimSuffix(string(request), "}") + `,"cursor":"epoch:2"}`)
	result, rpcErr = g.handleMCPEventRequest(ctx, "events/subscribe", request)
	if rpcErr != nil || result.(map[string]any)["cursor"] != "epoch:2" || result.(map[string]any)["truncated"] != true || len(entry.state.Pending) != 1 {
		t.Fatalf("expired replay failed: %v %v", result, rpcErr)
	}
	entry.state.Expires = time.Now().Add(-time.Second)
	request = []byte(strings.ReplaceAll(string(request), "epoch:2", "epoch:99"))
	result, rpcErr = g.handleMCPEventRequest(ctx, "events/subscribe", request)
	if rpcErr != nil || result.(map[string]any)["cursor"] != "epoch:2" || reads != 1 || entry.state.Pending[0].ID != "n5" {
		t.Fatal("refresh skipped unacknowledged outbox", result, rpcErr)
	}
}

func TestMCPEventStorageLockPreventsDuplicateDeliveryProcesses(t *testing.T) {
	g, ctx := gatewayMCPEventsFixture(t)
	verifications := 0
	g.mcpEvents.client = gatewayMCPEventTestReceiver(&verifications, func(*http.Request, []byte) int { return 204 })
	if _, err := g.handleMCPEventRequest(ctx, "events/subscribe", gatewayMCPSubscribeRequest(gatewayMCPEventsSecret("a"), `{"robot_id":"alpha"}`)); err != nil {
		t.Fatal(err)
	}
	other, err := NewRobotGateway(g.cfg, g.connect)
	if err != nil {
		t.Fatal(err)
	}
	if err = other.mcpEvents.start(context.Background()); err == nil {
		other.mcpEvents.close()
		t.Fatal("two gateways acquired same subscription storage")
	}
	g.mcpEvents.close()
	if err = other.mcpEvents.start(context.Background()); err != nil {
		t.Fatal("storage lock not released", err)
	}
	other.mcpEvents.close()
}

func TestMCPEventExpiredSubscriptionsDoNotExhaustQuota(t *testing.T) {
	g, ctx := gatewayMCPEventsFixture(t)
	for i := 0; i < 128; i++ {
		id := fmt.Sprintf("old_%d", i)
		g.mcpEvents.entries[id] = &gatewayMCPEventEntry{state: gatewayMCPEventSubscription{ID: id, Expires: time.Now().Add(-time.Hour)}, wake: make(chan struct{}, 1)}
	}
	verifications := 0
	g.mcpEvents.client = gatewayMCPEventTestReceiver(&verifications, func(*http.Request, []byte) int { return 204 })
	if _, err := g.handleMCPEventRequest(ctx, "events/subscribe", gatewayMCPSubscribeRequest(gatewayMCPEventsSecret("a"), `{"robot_id":"alpha"}`)); err != nil {
		t.Fatal(err)
	}
	if len(g.mcpEvents.entries) != 1 {
		t.Fatal("expired subscriptions exhausted quota")
	}
}

func TestMCPEventStateCannotFollowRemappedRobotIdentity(t *testing.T) {
	g, _ := gatewayMCPEventsFixture(t)
	config := g.cfg
	config.Robots = append([]GatewayRobot(nil), config.Robots...)
	config.Robots[0].Device = "different-device.local:50052"
	other, err := NewRobotGateway(config, g.connect)
	if err != nil {
		t.Fatal(err)
	}
	if other.mcpEvents.path == g.mcpEvents.path {
		t.Fatal("subscriptions followed a remapped device ID")
	}
}
