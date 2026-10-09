# Chat watches PR A: leased campaigns, implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a client run a camera detector on the device that notifies without recording anything and that the device removes by itself once the client stops renewing its lease.

**Architecture:** A campaign gains an optional `lease`. A leased campaign is validated as notify-only. The agent keeps its deadline in memory, renews it through a new `CampaignRenew` RPC, removes it through `CampaignRemove`, on expiry in the existing 5 s inference reconcile loop, and at boot. Its inference job takes a notify-only path: no application records, no model-input ledger, no episode, journal-only notifications. Every detection notification gains the top five `{label, score}` detections. The Python worker survives a detector exception.

**Tech Stack:** Go 1.27 (agent, `go/internal/agent/data` and `go/internal/agent/services`), protobuf/gRPC (`Proto/wendy/agent/services/v2/data_service.proto`, Go generation only), Python 3.12 worker (`go/internal/agent/inference/worker.py`, `unittest`).

**Spec:** `specs/2026-10-05-chat-watches-design.md`, §5 (PR A), §9–§12. Read §5 before starting any task.

## Global Constraints

- Lease: a Go duration string from `15s` through `10m` (`MinCampaignLease`, `MaxCampaignLease`).
- A leased campaign is notify-only: `inference` present and enabled, `notify.on: detection`, no `notify.webhook`; `capture`, `upload`, `retention`, `export`, `models` and `privacy` absent; every source a camera with no `capture` or `calibration_revision`.
- Campaigns without `lease` validate and hash exactly as today. Their revisions must not change.
- Removal happens at most lease + 5 s after the last renewal (the reconcile loop runs every 5 s).
- Deadlines live in memory only. A leased plan without a deadline is expired, so a restarted agent deletes every leased plan before it starts any inference job.
- `CampaignRenew` and `CampaignRemove` act only on leased campaigns: FAILED_PRECONDITION for an ordinary one, NOT_FOUND once removed or expired, INVALID_ARGUMENT for a malformed name.
- A redeploy that adds or removes a lease is FAILED_PRECONDITION.
- Leased campaigns deliver to the device's notification journal only. Never Cloud, never a webhook.
- `detections`: the five highest-scored detections that passed the label and threshold filter, `{label, score}` only, on every detection notification. A journal entry stays within 4096 bytes.
- The worker turns a detector exception into a `source_error` with the message cut to 512 UTF-8 bytes, and keeps serving.
- Generated code: commit only `go/proto/gen/agentpb/v2/data_service.pb.go` and `data_service_grpc.pb.go`; restore every other regenerated file.
- Run `gofmt -l .` from `go/` before every push. Branch prefix `ed/`. Commits authored as `24462281+EBro912@users.noreply.github.com` (already configured in this worktree).
- Joannis wrote the campaign and inference code and reviews this PR (spec D6).

## Review Focus

1. **Manual trigger of a leased campaign.** `wendy data campaign trigger chat-…` must refuse and open no episode; otherwise D3 (no recording) is broken by one command. Test: Task 5, `TestLeasedCampaignCannotBeTriggered`.
2. **A renewal that arrives after the deadline but before the reconcile pass.** It must be NOT_FOUND, never a revival of a watch whose client already lost it. Test: Task 3, `TestRenewAfterDeadlineIsNotFound`.
3. **Long or escaped labels push a detection notification past 4096 bytes.** The entry must survive with fewer detections, not vanish. Test: Task 2, `TestNotificationDropsDetectionsToFitLimit`.
4. **A leased campaign's records reaching another campaign's episode later, through the pre-roll ring.** Never. Test: Task 5, `TestRecordDeviceEventSkipsEpisodesAndPreRoll`.
5. **`CampaignRemove` racing a reconcile pass.** Once Remove returns, the job is stopped and no pass restarts it. Test: Task 4, `TestCampaignRemoveStopsJobBeforeReturning`.

---

## File map

| File | Task | Responsibility |
|---|---|---|
| `go/internal/agent/data/campaign.go` | 1, 4 | `lease` field, leased validation, revision digest, `RemoveCampaign` |
| `go/internal/agent/data/campaign_lease_test.go` (new) | 1, 4, 5 | data-package tests for the above and for `RecordDeviceEvent` |
| `go/internal/agent/data/notifications.go`, `notifications_test.go` | 2 | `NotificationDetection`, `detections`, size trimming |
| `go/internal/agent/data/device_events.go` | 5 | `RecordDeviceEvent` |
| `Proto/wendy/agent/services/v2/data_service.proto` + generated Go | 3 | the two RPCs |
| `go/internal/agent/services/data_campaign_lease.go` (new) | 3, 4 | `campaignLeases`, `CampaignRenew`, `CampaignRemove`, `expireLeases` |
| `go/internal/agent/services/data_campaign_lease_test.go` (new) | 3, 4, 5 | fake clock and service tests |
| `go/internal/agent/services/data_service.go` | 3, 5 | `leases` field, deploy deadline and transition check, trigger refusal |
| `go/internal/agent/services/data_inference.go` | 2, 4, 5 | `detections`, reconcile serialization and expiry, notify-only path |
| `go/internal/agent/services/data_inference_test.go` | 2 | top-five test |
| `go/internal/agent/inference/worker.py`, `test_worker.py` | 6 | detector resilience |
| `go/internal/cli/assets/docs/clients/wendy-cli/commands/data.md` | 2, 5 | reference docs |

All commands below run from the worktree root `/Users/ethan/Documents/WendyAgent-chat-watches` unless they `cd`. Before Task 1, create the branch: `git checkout -b ed/chat-watches-leased-campaigns` (from `ed/chat-watches`, which carries the spec and this plan).

---

### Task 1: Campaign schema — `lease` and leased validation

**Files:**
- Modify: `go/internal/agent/data/campaign.go` (Campaign struct ~line 242, `planDigestInput` ~line 325, `validate` ~line 408)
- Create: `go/internal/agent/data/campaign_lease_test.go`

**Interfaces:**
- Produces: `Campaign.Lease string` (`json:"lease,omitempty" yaml:"lease,omitempty"`); `func (c Campaign) Leased() bool`; `func (c Campaign) LeaseDuration() time.Duration`; `const MinCampaignLease = 15 * time.Second`, `MaxCampaignLease = 10 * time.Minute`; test constant `leasedCampaignYAML` (campaign `chat-3fa91c0e-1`, lease `60s`, camera `/dev/video0`).

- [ ] **Step 1: Write the failing tests**

Create `go/internal/agent/data/campaign_lease_test.go`:

```go
package data

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// leasedCampaignYAML is the shape a chat watch deploys (spec §5.1).
const leasedCampaignYAML = `version: 1
name: chat-3fa91c0e-1
lease: 60s
sources:
  - camera: /dev/video0
inference:
  model: PekingU/rtdetr_r18vd
  revision: ac77a11ff0170a41b771c03264987f8ce2b0d753
  labels: [person]
  threshold: 0.5
  rate: 2
  event: chat-3fa91c0e-1.detected
  clear_after: 5s
  cooldown: 30s
notify:
  on: detection
`

func TestLeasedCampaignParses(t *testing.T) {
	campaign, err := ParseCampaign([]byte(leasedCampaignYAML))
	if err != nil {
		t.Fatal(err)
	}
	if !campaign.Leased() || campaign.LeaseDuration() != 60*time.Second {
		t.Fatalf("lease not parsed: %q", campaign.Lease)
	}
	for _, lease := range []string{"15s", "10m"} {
		raw := strings.Replace(leasedCampaignYAML, "lease: 60s", "lease: "+lease, 1)
		if _, err := ParseCampaign([]byte(raw)); err != nil {
			t.Fatalf("lease %s is in range but was rejected: %v", lease, err)
		}
	}
}

func TestLeasedCampaignValidation(t *testing.T) {
	cases := []struct{ name, old, new, want string }{
		{"lease below minimum", "lease: 60s", "lease: 14s", "lease must be a duration from 15s through 10m0s"},
		{"lease above maximum", "lease: 60s", "lease: 10m1s", "lease must be a duration from 15s through 10m0s"},
		{"lease without unit", "lease: 60s", `lease: "60"`, "lease must be a duration from 15s through 10m0s"},
		{"audio source", "  - camera: /dev/video0\n", "  - camera: /dev/video0\n  - audio: default\n", "sources[1]: a leased campaign selects cameras only"},
		{"source capture", "  - camera: /dev/video0\n", "  - camera: /dev/video0\n    capture: {mode: continuous}\n", "sources[0]: a leased campaign records nothing, so it takes no capture or calibration_revision"},
		{"capture block", "notify:\n", "capture: {buffer: 1s, after_trigger: 1s, triggers: [{event: go}]}\nnotify:\n", "a leased campaign records nothing, so it takes no capture"},
		{"upload block", "notify:\n", "upload: {when: wifi}\nnotify:\n", "a leased campaign records nothing, so it takes no upload"},
		{"retention block", "notify:\n", "retention: {local_quota: 1GiB}\nnotify:\n", "a leased campaign records nothing, so it takes no retention"},
		{"export block", "notify:\n", "export: {annotation: cvat}\nnotify:\n", "a leased campaign records nothing, so it takes no export"},
		{"models", "notify:\n", "models: {detector: v1}\nnotify:\n", "a leased campaign records nothing, so it takes no models"},
		{"privacy", "notify:\n", "privacy: [{name: blur}]\nnotify:\n", "a leased campaign records nothing, so it takes no privacy"},
		{"inference disabled", "  cooldown: 30s\n", "  cooldown: 30s\n  enabled: false\n", "a leased campaign needs an enabled inference block"},
		{"notify episode_committed", "  on: detection", "  on: episode_committed", "a leased campaign needs notify.on: detection"},
		{"notify event", "  on: detection", "  on: event\n  event: go", "a leased campaign needs notify.on: detection"},
		{"webhook", "  on: detection\n", "  on: detection\n  webhook: https://example.com/hook\n", "remove notify.webhook"},
		{"unknown notify key", "  on: detection\n", "  on: detection\n  channel: x\n", "unknown immediate notification fields: channel"},
		{"inference still validated", "  rate: 2\n", "  rate: 0\n", "inference.rate must be in (0, 30]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(leasedCampaignYAML, tc.old, tc.new, 1)
			if raw == leasedCampaignYAML {
				t.Fatalf("replacement %q not found in the fixture", tc.old)
			}
			_, err := ParseCampaign([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// A client may build the YAML by marshalling a Campaign, which writes the
// empty capture, upload and export blocks out. Those count as absent.
func TestLeasedCampaignRoundTripsThroughYAML(t *testing.T) {
	campaign, err := ParseCampaign([]byte(leasedCampaignYAML))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(campaign)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseCampaign(raw)
	if err != nil {
		t.Fatalf("a marshalled leased campaign no longer parses: %v\n%s", err, raw)
	}
	if again.Revision != campaign.Revision {
		t.Fatal("round trip changed the revision")
	}
}

func TestLeaseChangesRevisionOnlyWhenSet(t *testing.T) {
	short, err := ParseCampaign([]byte(leasedCampaignYAML))
	if err != nil {
		t.Fatal(err)
	}
	long, err := ParseCampaign([]byte(strings.Replace(leasedCampaignYAML, "lease: 60s", "lease: 120s", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if short.Revision == long.Revision {
		t.Fatal("changing the lease kept the revision")
	}
	people, err := ParseCampaign(peopleCampaign(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := people.planDigestInput()["lease"]; ok {
		t.Fatal("a campaign without a lease hashes a lease key, which would change every deployed revision")
	}
}
```

(`peopleCampaign` is defined in `inference_test.go` in the same package.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd go && go test ./internal/agent/data/ -run 'Lease' -v`
Expected: build failure: `campaign.Leased undefined` (and `LeaseDuration`).

- [ ] **Step 3: Add the field, constants and helpers**

In `campaign.go`, in `type Campaign struct`, after the `Fleet` line add:

```go
	// Lease makes this a leased campaign: a notify-only detector that a client
	// keeps alive by renewing it. The agent removes it once the lease lapses.
	Lease string `json:"lease,omitempty" yaml:"lease,omitempty"`
```

After `const CampaignVersion = 1` add:

```go
// A leased campaign's lease is a duration from MinCampaignLease through
// MaxCampaignLease.
const (
	MinCampaignLease = 15 * time.Second
	MaxCampaignLease = 10 * time.Minute
)
```

After the `ParseCampaign` function add:

```go
// Leased reports whether the campaign has a lease.
func (c Campaign) Leased() bool { return c.Lease != "" }

// LeaseDuration is the campaign's lease, or zero for a campaign without one.
func (c Campaign) LeaseDuration() time.Duration {
	d, _ := time.ParseDuration(c.Lease)
	return d
}
```

- [ ] **Step 4: Branch validation for leased campaigns**

In `validate()`, directly after the `for i, source := range c.Sources { … }` loop closes and before `buffer, err := time.ParseDuration(c.Capture.Buffer)`, insert:

```go
	if c.Leased() {
		return c.validateLeased()
	}
```

Then move the notify checks at the end of `validate()` into their own function, unchanged. Replace everything from the line `if c.Notify != nil && (c.Notify.On == NotifyOnDetection || c.Notify.On == NotifyOnEvent) {` through the final `return nil` of `validate()` with:

```go
	return c.validateNotify()
}

// validateNotify checks the notify block, which ordinary and leased campaigns
// share.
func (c Campaign) validateNotify() error {
	if c.Notify != nil && (c.Notify.On == NotifyOnDetection || c.Notify.On == NotifyOnEvent) {
		if len(c.Notify.UnknownKeys) > 0 {
			return fmt.Errorf("unknown immediate notification fields: %s", strings.Join(c.Notify.UnknownKeys, ", "))
		}
		if c.Notify.On == NotifyOnDetection && c.Inference == nil {
			return errors.New("notify.on: detection requires inference")
		}

		if c.Notify.Webhook != "" {
			endpoint, err := url.Parse(c.Notify.Webhook)
			if err != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Fragment != "" || len(c.Notify.Webhook) > 2048 {
				return errors.New("notify.webhook must be an absolute HTTP(S) URL without userinfo or fragment, at most 2048 bytes")
			}
		}
	} else if c.Notify != nil && c.Notify.Webhook != "" {
		return errors.New("notify.webhook requires notify.on: detection or event")
	}

	if c.Notify != nil {
		if c.Notify.On == NotifyOnEvent && !inferenceEventRE.MatchString(c.Notify.Event) {
			return errors.New("notify.event must use 1..128 letters, numbers, dots, hyphens or underscores")
		}
		if c.Notify.On != NotifyOnEvent && c.Notify.Event != "" {
			return errors.New("notify.event requires notify.on: event")
		}
	}

	if c.Notify != nil && c.Notify.On != NotifyOnEpisodeCommitted && c.Notify.On != NotifyOnDetection && c.Notify.On != NotifyOnEvent {
		return fmt.Errorf("notify.on %q is not supported: %w", c.Notify.On, ErrUnsupportedNotifyOn)
	}
	return nil
}

// validateLeased checks a leased campaign. It is notify-only: it runs
// inference and writes notifications but records nothing, so everything that
// configures recording, upload or export must be absent (spec §5.1).
func (c Campaign) validateLeased() error {
	lease, err := time.ParseDuration(c.Lease)
	if err != nil || lease < MinCampaignLease || lease > MaxCampaignLease {
		return fmt.Errorf("lease must be a duration from %s through %s", MinCampaignLease, MaxCampaignLease)
	}
	for i, source := range c.Sources {
		if source.Camera == "" {
			return fmt.Errorf("sources[%d]: a leased campaign selects cameras only", i)
		}
		if source.Capture != nil || source.Calibration != "" {
			return fmt.Errorf("sources[%d]: a leased campaign records nothing, so it takes no capture or calibration_revision", i)
		}
	}
	recording := []struct {
		field string
		set   bool
	}{
		{"capture", c.Capture.Buffer != "" || c.Capture.Drain != "" || c.Capture.AfterTrigger != "" || len(c.Capture.Triggers) > 0},
		{"upload", c.Upload != (CampaignUpload{})},
		{"retention", c.Retention != (CampaignRetention{})},
		{"export", c.Export != (CampaignExport{})},
		{"models", len(c.Models) > 0},
		{"privacy", len(c.Privacy) > 0},
	}
	for _, block := range recording {
		if block.set {
			return fmt.Errorf("a leased campaign records nothing, so it takes no %s", block.field)
		}
	}
	if !c.Inference.IsEnabled() {
		return errors.New("a leased campaign needs an enabled inference block")
	}
	if c.Notify == nil || c.Notify.On != NotifyOnDetection {
		return errors.New("a leased campaign needs notify.on: detection")
	}
	if c.Notify.Webhook != "" {
		return errors.New("a leased campaign notifies through the device's notification journal only; remove notify.webhook")
	}
	if err := c.Inference.validate(); err != nil {
		return err
	}
	return c.validateNotify()
}
```

The block moved into `validateNotify` must be byte-for-byte the code that was at the end of `validate()`; the only change to `validate()` is that it now ends with `return c.validateNotify()`. Check with `git diff` that no condition changed.

- [ ] **Step 5: Put the lease in the revision, only when set**

In `planDigestInput`, after the `if c.Notify != nil { … }` block and before `if i := c.Inference; i != nil {`, add:

```go
	// The lease is plan content. Adding it only when set keeps the revisions of
	// campaigns without one, as backend and model_file do below.
	if c.Lease != "" {
		plan["lease"] = c.Lease
	}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd go && go test ./internal/agent/data/ -run 'Lease' -v`
Expected: PASS for all four tests, including every `TestLeasedCampaignValidation` subtest.

Run: `cd go && go test ./internal/agent/data/`
Expected: `ok` (existing validation and revision tests are unchanged).

- [ ] **Step 7: Commit**

```bash
git add go/internal/agent/data/campaign.go go/internal/agent/data/campaign_lease_test.go
git commit -m "feat(agent): add leased, notify-only campaigns to the campaign schema

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M9Sy7Q2qsNmQy7v8wtkdUD"
```

---

### Task 2: Detections in notifications

**Files:**
- Modify: `go/internal/agent/data/notifications.go`
- Modify: `go/internal/agent/data/notifications_test.go`
- Modify: `go/internal/agent/services/data_inference.go` (`detectionNotification` ~line 470 and its call in `run`)
- Modify: `go/internal/agent/services/data_inference_test.go`
- Modify: `go/internal/cli/assets/docs/clients/wendy-cli/commands/data.md` (Notifications section)

**Interfaces:**
- Produces: `data.NotificationDetection{Label string; Score float64}` (`json:"label"`, `json:"score"`); `CampaignNotification.Detections []NotificationDetection` (`json:"detections,omitempty"`); `func detectionNotification(campaign data.Campaign, source string, detections []inference.Detection) DetectionNotification` (signature change: was `count int`); `func notificationDetections([]inference.Detection) []data.NotificationDetection`.

- [ ] **Step 1: Write the failing data test**

Append to `go/internal/agent/data/notifications_test.go` (add `encoding/json` and `strings` to its imports):

```go
func TestNotificationDetectionsRoundTrip(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	notification := CampaignNotification{ID: uuid.NewString(), Event: "chat-1.detected", Campaign: "chat-1", SourceID: "camera", Count: 1,
		Detections: []NotificationDetection{{Label: "person", Score: 0.91}}, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := manager.RecordNotification(notification); err != nil {
		t.Fatal(err)
	}
	items, _, _, err := manager.Notifications("", "", "", true)
	if err != nil || len(items) != 1 {
		t.Fatalf("notification not retained: %v %v", items, err)
	}
	if len(items[0].Detections) != 1 || items[0].Detections[0] != (NotificationDetection{Label: "person", Score: 0.91}) {
		t.Fatalf("detections did not round-trip: %+v", items[0].Detections)
	}
}

// Labels may be up to 128 bytes and json escapes '<' as six bytes, so five
// detections can exceed the entry limit. The entry must survive with fewer.
func TestNotificationDropsDetectionsToFitLimit(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	label := strings.Repeat("<", 128)
	notification := CampaignNotification{ID: uuid.NewString(), Event: "e", Campaign: "c", SourceID: "camera", Count: 5, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano)}
	for i := 0; i < 5; i++ {
		notification.Detections = append(notification.Detections, NotificationDetection{Label: label, Score: 0.9 - float64(i)/10})
	}
	if err := manager.RecordNotification(notification); err != nil {
		t.Fatalf("an oversized detection list cost the whole notification: %v", err)
	}
	items, _, _, err := manager.Notifications("", "", "", true)
	if err != nil || len(items) != 1 {
		t.Fatalf("notification not retained: %v %v", items, err)
	}
	got := items[0].Detections
	if len(got) == 0 || len(got) >= 5 {
		t.Fatalf("detections were not trimmed to fit: kept %d", len(got))
	}
	if got[0].Score != 0.9 {
		t.Fatal("trimming dropped the highest-scored detection")
	}
	if raw, _ := json.Marshal(items[0]); len(raw) > 4096 {
		t.Fatalf("stored entry is %d bytes", len(raw))
	}
}
```

- [ ] **Step 2: Write the failing services test**

Append to `go/internal/agent/services/data_inference_test.go`:

```go
func TestDetectionNotificationKeepsTopFiveByScore(t *testing.T) {
	campaign, err := data.ParseCampaign(inferenceTestYAML(t))
	if err != nil {
		t.Fatal(err)
	}
	var detections []inference.Detection
	for _, score := range []float64{.5, .9, .7, .95, .6, .8, .55} {
		detections = append(detections, inference.Detection{Label: "person", Score: score, Box: [4]float64{1, 2, 3, 4}})
	}
	notification := detectionNotification(campaign, "v4l2:/dev/video0", detections)
	if notification.Count != 7 {
		t.Fatalf("count = %d, want every accepted detection", notification.Count)
	}
	want := []float64{.95, .9, .8, .7, .6}
	if len(notification.Detections) != len(want) {
		t.Fatalf("kept %d detections, want 5", len(notification.Detections))
	}
	for i, detection := range notification.Detections {
		if detection.Label != "person" || detection.Score != want[i] {
			t.Fatalf("detections[%d] = %+v, want person %.2f", i, detection, want[i])
		}
	}
}
```

Also, in `TestAgentInferenceAllCamerasRecordAndNotify`, after `notification1 := receiveInference(t, sender.requests)` add:

```go
	if len(notification1.Detections) != 1 || notification1.Detections[0] != (data.NotificationDetection{Label: "person", Score: .99}) {
		t.Fatalf("detection notification lacks its detections: %+v", notification1.Detections)
	}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd go && go test ./internal/agent/data/ -run 'NotificationDetections|NotificationDrops' -v`
Expected: build failure: `undefined: NotificationDetection`.

- [ ] **Step 4: Implement the data side**

In `notifications.go`, above `type CampaignNotification struct` add:

```go
// NotificationDetection summarizes one detection for a notification: its label
// and score, never a box or an image.
type NotificationDetection struct {
	Label string  `json:"label"`
	Score float64 `json:"score"`
}
```

In `CampaignNotification`, between `OccurredAt` and `Sequence`, add:

```go
	// Detections holds up to five of the detections behind a detection
	// notification, highest score first. Older clients ignore it.
	Detections []NotificationDetection `json:"detections,omitempty"`
```

In `RecordNotification`, replace

```go
	raw, err := json.Marshal(notification)
	if err != nil || len(raw) > 4096 {
		return fmt.Errorf("notification exceeds limit")
	}
```

with

```go
	raw, err := json.Marshal(notification)
	// Detections are a summary: drop the lowest-scored ones, not the
	// notification, when long labels push an entry past the limit. Cloud and
	// webhook delivery send the request as it was built.
	for err == nil && len(raw) > 4096 && len(notification.Detections) > 0 {
		notification.Detections = notification.Detections[:len(notification.Detections)-1]
		raw, err = json.Marshal(notification)
	}
	if err != nil || len(raw) > 4096 {
		return fmt.Errorf("notification exceeds limit")
	}
```

- [ ] **Step 5: Implement the services side**

In `data_inference.go`, add `"cmp"` and `"slices"` to the imports, and replace `detectionNotification` with:

```go
func detectionNotification(campaign data.Campaign, source string, detections []inference.Detection) DetectionNotification {
	return DetectionNotification{ID: uuid.NewString(), Event: campaign.Inference.Event, Campaign: campaign.Name, SourceID: source, Model: campaign.Inference.Model, Revision: campaign.Inference.Revision, Count: len(detections), Detections: notificationDetections(detections), OccurredAt: time.Now().UTC().Format(time.RFC3339Nano)}
}

// notificationDetections keeps the five highest-scored detections, which keeps
// a journal entry well under its 4096-byte limit (spec §5.5).
func notificationDetections(detections []inference.Detection) []data.NotificationDetection {
	sorted := slices.Clone(detections)
	slices.SortStableFunc(sorted, func(a, b inference.Detection) int { return cmp.Compare(b.Score, a.Score) })
	sorted = sorted[:min(len(sorted), 5)]
	out := make([]data.NotificationDetection, 0, len(sorted))
	for _, detection := range sorted {
		out = append(out, data.NotificationDetection{Label: detection.Label, Score: detection.Score})
	}
	return out
}
```

In `run`, change `request := detectionNotification(j.campaign, result.SourceID, len(detections))` to `request := detectionNotification(j.campaign, result.SourceID, detections)`.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd go && go test ./internal/agent/data/ ./internal/agent/services/ -run 'Notification|AllCamerasRecordAndNotify' -v`
Expected: PASS.

- [ ] **Step 7: Document `detections`**

In `data.md`, in the `### Notifications` section, after the paragraph that ends `…each with a 10-second timeout and the same event UUID.`, add:

```markdown
A `detection` notification carries `detections`: up to five `{label, score}`
pairs that passed the label and threshold filter, highest score first. It never
carries boxes or images. Readers that predate the field ignore it.
```

- [ ] **Step 8: Commit**

```bash
git add go/internal/agent/data/notifications.go go/internal/agent/data/notifications_test.go go/internal/agent/services/data_inference.go go/internal/agent/services/data_inference_test.go go/internal/cli/assets/docs/clients/wendy-cli/commands/data.md
git commit -m "feat(agent): carry the top five detections in detection notifications

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M9Sy7Q2qsNmQy7v8wtkdUD"
```

---

### Task 3: Lease deadlines, `CampaignRenew` and the RPCs

**Files:**
- Modify: `Proto/wendy/agent/services/v2/data_service.proto`
- Regenerate: `go/proto/gen/agentpb/v2/data_service.pb.go`, `go/proto/gen/agentpb/v2/data_service_grpc.pb.go`
- Create: `go/internal/agent/services/data_campaign_lease.go`
- Modify: `go/internal/agent/services/data_service.go` (`DataService` struct ~line 55, `CampaignDeploy` ~line 457)
- Create: `go/internal/agent/services/data_campaign_lease_test.go`

**Interfaces:**
- Consumes: `Campaign.Leased()`, `Campaign.LeaseDuration()` (Task 1).
- Produces: proto messages `DataCampaignRenewRequest{name}`, `DataCampaignRenewResponse{expires_unix_nanos}`, `DataCampaignRemoveRequest{name}`, `DataCampaignRemoveResponse{}`; `type campaignLeases struct` with `now func() time.Time` (nil means `time.Now`), `extend(name string, lease time.Duration) time.Time`, `renew(name string, lease time.Duration) (time.Time, bool)`, `forget(name string)`; `DataService.leases campaignLeases` (a value; its zero value works); `func (s *DataService) CampaignRenew(context.Context, *agentpbv2.DataCampaignRenewRequest) (*agentpbv2.DataCampaignRenewResponse, error)`. Test helpers: `leaseTestClock`, `leaseTestVideo`, `leasedTestYAML(name, lease string) []byte`, `newLeaseTest(t, before func(*data.Manager)) *leaseTest`, `deployLeased(t, service, name, lease)`.

- [ ] **Step 1: Add the RPCs to the proto**

In `data_service.proto`, replace the comment block above `service DataService` that starts `// The four Campaign methods in particular` with:

```proto
// The Campaign methods in particular are command-line-facing. Deploy, list,
// inspect and trigger are what `wendy data campaign deploy`, `wendy data
// campaign list`, `wendy data campaign inspect` and `wendy data campaign
// trigger` call over that mutually authenticated channel. Renew and remove keep
// a leased campaign alive and end it; chat watches in `wendy mcp serve` use
// them. They are operator commands for managing campaigns on a device, not an
// application programming interface for apps.
```

After `rpc CampaignTrigger(DataCampaignTriggerRequest) returns (DataEpisode);` add:

```proto
  // Pushes a leased campaign's deadline to now + lease. NOT_FOUND once it was
  // removed or its lease lapsed; FAILED_PRECONDITION for a campaign without a
  // lease.
  rpc CampaignRenew(DataCampaignRenewRequest) returns (DataCampaignRenewResponse);
  // Stops a leased campaign and deletes it, returning after its inference has
  // stopped. NOT_FOUND if already gone; FAILED_PRECONDITION for a campaign
  // without a lease.
  rpc CampaignRemove(DataCampaignRemoveRequest) returns (DataCampaignRemoveResponse);
```

After `message DataCampaignsResponse { repeated DataCampaign campaigns = 1; }` add:

```proto
message DataCampaignRenewRequest { string name = 1; }
// The new deadline, in Unix nanoseconds on the device's clock.
message DataCampaignRenewResponse { int64 expires_unix_nanos = 1; }
message DataCampaignRemoveRequest { string name = 1; }
message DataCampaignRemoveResponse {}
```

- [ ] **Step 2: Regenerate and keep only the data service files**

```bash
cd go && export PATH="$PATH:$(go env GOPATH)/bin" && make proto && cd ..
git diff --name-only -- go/proto/gen/ | grep -v '^go/proto/gen/agentpb/v2/data_service' | xargs git checkout --
git status --short go/proto/gen/
```

Expected: exactly ` M go/proto/gen/agentpb/v2/data_service.pb.go` and ` M go/proto/gen/agentpb/v2/data_service_grpc.pb.go`. The local protoc is newer than the one that generated `main`, so these two files also get a version-comment change; that is expected. Any untracked file under `go/proto/gen/` means something else changed: stop and investigate.

Run: `cd go && go build ./internal/agent/... ./cmd/wendy-agent/`
Expected: builds (the new RPCs answer UNIMPLEMENTED through the embedded `UnimplementedDataServiceServer` until implemented).

- [ ] **Step 3: Write the failing tests**

Create `go/internal/agent/services/data_campaign_lease_test.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they fail**

Run: `cd go && go test ./internal/agent/services/ -run 'Lease|Renew' -v`
Expected: build failure: `lt.service.leases undefined (type *DataService has no field or method leases)`. (`CampaignRenew` already resolves, to the embedded `UnimplementedDataServiceServer`.)

- [ ] **Step 5: Implement the lease table and `CampaignRenew`**

Create `go/internal/agent/services/data_campaign_lease.go`:

```go
package services

import (
	"context"
	"sync"
	"time"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// campaignLeases holds the deadlines of leased campaigns. They live in memory
// only: a leased plan without a deadline counts as expired, so a restarted
// agent removes the leased plans of its previous run before it starts any
// inference (spec §5.3). The zero value is ready to use.
type campaignLeases struct {
	mu sync.Mutex
	// now is the clock; nil means time.Now. Tests set a fake one before
	// StartCampaignInference.
	now       func() time.Time
	deadlines map[string]time.Time
}

func (l *campaignLeases) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// extend sets name's deadline to now + lease and returns it.
func (l *campaignLeases) extend(name string, lease time.Duration) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.deadlines == nil {
		l.deadlines = map[string]time.Time{}
	}
	deadline := l.clock().Add(lease)
	l.deadlines[name] = deadline
	return deadline
}

// renew extends a lease that has not lapsed. It refuses a lapsed one, even in
// the seconds before the reconcile loop removes its campaign: renewing would
// revive a watch whose client has already lost it.
func (l *campaignLeases) renew(name string, lease time.Duration) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	if deadline, ok := l.deadlines[name]; !ok || !now.Before(deadline) {
		return time.Time{}, false
	}
	l.deadlines[name] = now.Add(lease)
	return l.deadlines[name], true
}

func (l *campaignLeases) forget(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.deadlines, name)
}

// CampaignRenew pushes a leased campaign's deadline to now + lease.
func (s *DataService) CampaignRenew(_ context.Context, req *agentpbv2.DataCampaignRenewRequest) (*agentpbv2.DataCampaignRenewResponse, error) {
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	campaign, err := s.manager.Campaign(req.GetName())
	if err != nil {
		return nil, dataStatusError(err)
	}
	if !campaign.Leased() {
		return nil, status.Errorf(codes.FailedPrecondition, "campaign %q has no lease", campaign.Name)
	}
	deadline, ok := s.leases.renew(campaign.Name, campaign.LeaseDuration())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "campaign %q lease expired", campaign.Name)
	}
	return &agentpbv2.DataCampaignRenewResponse{ExpiresUnixNanos: deadline.UnixNano()}, nil
}
```

- [ ] **Step 6: Start leases on deploy and refuse lease transitions**

In `data_service.go`, in `type DataService struct`, after `deploymentMu sync.Mutex` add:

```go
	// leases holds leased campaigns' deadlines. deploymentMu orders every
	// change to them with the plan files they belong to.
	leases campaignLeases
```

In `CampaignDeploy`, after the `if parsed.Notify != nil && parsed.Notify.On == data.NotifyOnEvent && s.inference == nil { … }` block and before `campaign, err := s.manager.DeployCampaign(req.GetCampaignYaml())`, add:

```go
	// A lease is fixed for a campaign's life: a redeploy must not turn a watch
	// into a recording campaign, or the reverse.
	if existing, err := s.manager.Campaign(parsed.Name); err == nil && existing.Leased() != parsed.Leased() {
		if existing.Leased() {
			return nil, status.Errorf(codes.FailedPrecondition, "campaign %q is leased; a redeploy cannot remove its lease", parsed.Name)
		}
		return nil, status.Errorf(codes.FailedPrecondition, "campaign %q is not leased; a redeploy cannot add a lease", parsed.Name)
	}
```

After the `if err != nil { return nil, status.Error(codes.InvalidArgument, err.Error()) }` that follows `s.manager.DeployCampaign`, add:

```go
	if campaign.Leased() {
		// Deploying or redeploying starts the lease afresh.
		s.leases.extend(campaign.Name, campaign.LeaseDuration())
	}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `cd go && go test ./internal/agent/services/ -run 'Lease|Renew' -race -v`
Expected: PASS for all five tests, no race reports.

Run: `cd go && go test ./internal/agent/services/`
Expected: `ok`.

- [ ] **Step 8: Commit**

```bash
git add Proto/wendy/agent/services/v2/data_service.proto go/proto/gen/agentpb/v2/data_service.pb.go go/proto/gen/agentpb/v2/data_service_grpc.pb.go go/internal/agent/services/data_campaign_lease.go go/internal/agent/services/data_campaign_lease_test.go go/internal/agent/services/data_service.go
git commit -m "feat(agent): lease deadlines and CampaignRenew for leased campaigns

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M9Sy7Q2qsNmQy7v8wtkdUD"
```

---

### Task 4: Expiry, `CampaignRemove` and the boot sweep

**Files:**
- Modify: `go/internal/agent/data/campaign.go` (add `RemoveCampaign` after `Campaigns`)
- Modify: `go/internal/agent/data/campaign_lease_test.go`
- Modify: `go/internal/agent/services/data_campaign_lease.go`
- Modify: `go/internal/agent/services/data_inference.go` (`campaignInferenceManager`, `StartCampaignInference`, `reconcile`)
- Modify: `go/internal/agent/services/data_campaign_lease_test.go`

**Interfaces:**
- Consumes: `campaignLeases`, `DataService.leases`, `newLeaseTest`, `deployLeased`, `renew`, `leaseTestVideo.closed` (Task 3); `leasedCampaignYAML` (Task 1).
- Produces: `func (m *Manager) RemoveCampaign(name string) error` (ErrInvalidCampaignName for a malformed name, an `os.ErrNotExist` error when absent); `campaignLeases.expired(name string) bool`; `func (s *DataService) expireLeases()`; `func (s *DataService) CampaignRemove(context.Context, *agentpbv2.DataCampaignRemoveRequest) (*agentpbv2.DataCampaignRemoveResponse, error)`; `campaignInferenceManager.ctx`, `.reconcileMu`, `func (m *campaignInferenceManager) reconcileNow()`.

Lock order, which this task must keep: `reconcileMu` before `deploymentMu`. Nothing may wait for a job to finish while holding `deploymentMu`, because a job takes `deploymentMu` in `triggerInference` (and, after Task 5, in `detectionAccepted`).

- [ ] **Step 1: Write the failing data test**

Append to `go/internal/agent/data/campaign_lease_test.go` (add `errors` and `os` to its imports):

```go
func TestRemoveCampaignDeletesThePlan(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.DeployCampaign([]byte(leasedCampaignYAML)); err != nil {
		t.Fatal(err)
	}
	if err := manager.RemoveCampaign("chat-3fa91c0e-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Campaign("chat-3fa91c0e-1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plan still readable after removal: %v", err)
	}
	if err := manager.RemoveCampaign("chat-3fa91c0e-1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removing twice: %v, want not-exist", err)
	}
	if err := manager.RemoveCampaign("../escape"); !errors.Is(err, ErrInvalidCampaignName) {
		t.Fatalf("malformed name: %v", err)
	}
}
```

- [ ] **Step 2: Write the failing services tests**

Append to `go/internal/agent/services/data_campaign_lease_test.go` (add `errors` and `os` to its imports):

```go
func TestLeaseExpiryRemovesCampaignAndFreesCamera(t *testing.T) {
	lt := newLeaseTest(t, nil)
	deployLeased(t, lt.service, "chat-1", "15s")
	session := receiveInference(t, lt.factory.sessions)
	receiveInference(t, session.inputs)

	lt.clock.Advance(14 * time.Second)
	lt.service.inference.reconcileNow()
	if _, err := lt.service.manager.Campaign("chat-1"); err != nil {
		t.Fatalf("removed before its lease lapsed: %v", err)
	}

	lt.clock.Advance(time.Second)
	lt.service.inference.reconcileNow()
	if _, err := lt.service.manager.Campaign("chat-1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired campaign still deployed: %v", err)
	}
	receiveInference(t, session.closed)
	if id := receiveInference(t, lt.video.closed); id != "v4l2:/dev/video0" {
		t.Fatalf("closed subscription %q", id)
	}
	if _, err := lt.service.CampaignInspect(context.Background(), &agentpbv2.DataCampaignInspectRequest{Name: "chat-1"}); status.Code(err) != codes.NotFound {
		t.Fatalf("inspect after expiry: %v, want NotFound", err)
	}
	if _, err := renew(lt.service, "chat-1"); status.Code(err) != codes.NotFound {
		t.Fatalf("renew after expiry: %v, want NotFound", err)
	}
}

func TestCampaignRemoveStopsJobBeforeReturning(t *testing.T) {
	lt := newLeaseTest(t, nil)
	deployLeased(t, lt.service, "chat-1", "60s")
	session := receiveInference(t, lt.factory.sessions)
	receiveInference(t, session.inputs)

	if _, err := lt.service.CampaignRemove(context.Background(), &agentpbv2.DataCampaignRemoveRequest{Name: "chat-1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.closed:
	default:
		t.Fatal("CampaignRemove returned before the model process stopped")
	}
	select {
	case <-lt.video.closed:
	default:
		t.Fatal("CampaignRemove returned before the camera subscription closed")
	}
	// No later pass may start the job again once its plan is gone.
	lt.service.inference.reconcileNow()
	select {
	case <-lt.factory.sessions:
		t.Fatal("a removed campaign's job started again")
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := lt.service.CampaignRemove(context.Background(), &agentpbv2.DataCampaignRemoveRequest{Name: "chat-1"}); status.Code(err) != codes.NotFound {
		t.Fatalf("second remove: %v, want NotFound", err)
	}
	if _, err := renew(lt.service, "chat-1"); status.Code(err) != codes.NotFound {
		t.Fatalf("renew after remove: %v, want NotFound", err)
	}
}

func TestRemoveRejectsOrdinaryCampaigns(t *testing.T) {
	lt := newLeaseTest(t, nil)
	if _, err := lt.service.CampaignDeploy(context.Background(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: inferenceTestYAML(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := lt.service.CampaignRemove(context.Background(), &agentpbv2.DataCampaignRemoveRequest{Name: "people-all-cameras"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("removing an ordinary campaign: %v, want FailedPrecondition", err)
	}
	if _, err := lt.service.manager.Campaign("people-all-cameras"); err != nil {
		t.Fatalf("the refused remove deleted the plan: %v", err)
	}
	if _, err := lt.service.CampaignRemove(context.Background(), &agentpbv2.DataCampaignRemoveRequest{Name: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("removing a missing campaign: %v, want NotFound", err)
	}
}

func TestRestartDeletesLeasedPlans(t *testing.T) {
	lt := newLeaseTest(t, func(manager *data.Manager) {
		// Plans written by the previous agent run.
		if _, err := manager.DeployCampaign(leasedTestYAML("chat-1", "60s")); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.DeployCampaign(inferenceTestYAML(t)); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := lt.service.manager.Campaign("chat-1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a leased plan survived the restart: %v", err)
	}
	if _, err := lt.service.manager.Campaign("people-all-cameras"); err != nil {
		t.Fatalf("the restart deleted an ordinary plan: %v", err)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd go && go test ./internal/agent/data/ -run RemoveCampaign -v`
Expected: build failure: `manager.RemoveCampaign undefined`.

Run: `cd go && go test ./internal/agent/services/ -run 'Expiry|Remove|Restart' -v`
Expected: build failure: `lt.service.inference.reconcileNow undefined`.

- [ ] **Step 4: Add `RemoveCampaign`**

In `campaign.go`, after the `Campaigns` function, add:

```go
// RemoveCampaign deletes a campaign's plan. Only leased campaigns are removed
// today, by expiry, by CampaignRemove and at boot; callers check that.
func (m *Manager) RemoveCampaign(name string) error {
	if name == "" || safeName(name) != name {
		return ErrInvalidCampaignName
	}
	m.campaignMu.Lock()
	defer m.campaignMu.Unlock()
	return os.Remove(filepath.Join(m.campaignDir(), name+".json"))
}
```

- [ ] **Step 5: Add expiry and `CampaignRemove`**

In `data_campaign_lease.go`, add `"errors"` and `"os"` to the imports, then add after `forget`:

```go
// expired reports whether name's lease has lapsed. A name without a deadline
// has lapsed: deadlines live in memory, so it is a plan from before the agent
// restarted.
func (l *campaignLeases) expired(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	deadline, ok := l.deadlines[name]
	return !ok || !l.clock().Before(deadline)
}
```

and at the end of the file:

```go
// expireLeases deletes every leased plan whose lease has lapsed. Reconcile
// passes call it before reading plans, so the same pass retires the deleted
// plan's job. The plan goes first: a detection that races the removal finds no
// current revision and sends nothing.
func (s *DataService) expireLeases() {
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	campaigns, err := s.manager.Campaigns()
	if err != nil {
		s.manager.Warnf("reading campaigns to expire leases: %v", err)
		return
	}
	for _, campaign := range campaigns {
		if !campaign.Leased() || !s.leases.expired(campaign.Name) {
			continue
		}
		if err := s.manager.RemoveCampaign(campaign.Name); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.manager.Warnf("removing leased campaign %q after its lease lapsed: %v", campaign.Name, err)
			continue
		}
		s.leases.forget(campaign.Name)
		s.manager.Warnf("leased campaign %q was removed because its lease lapsed or the agent restarted", campaign.Name)
	}
}

// CampaignRemove stops a leased campaign and deletes its plan. It returns once
// the campaign's model process and camera subscriptions have stopped.
func (s *DataService) CampaignRemove(_ context.Context, req *agentpbv2.DataCampaignRemoveRequest) (*agentpbv2.DataCampaignRemoveResponse, error) {
	if err := s.removeLeased(req.GetName()); err != nil {
		return nil, err
	}
	// With the plan gone, a reconcile pass retires the job and waits for it.
	if s.inference != nil {
		s.inference.reconcileNow()
	}
	return &agentpbv2.DataCampaignRemoveResponse{}, nil
}

// removeLeased deletes a leased campaign's plan and forgets its deadline. It
// releases deploymentMu before CampaignRemove waits for the job.
func (s *DataService) removeLeased(name string) error {
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	campaign, err := s.manager.Campaign(name)
	if err != nil {
		return dataStatusError(err)
	}
	if !campaign.Leased() {
		return status.Errorf(codes.FailedPrecondition, "campaign %q has no lease; only leased campaigns can be removed", name)
	}
	if err := s.manager.RemoveCampaign(name); err != nil {
		return dataStatusError(err)
	}
	s.leases.forget(name)
	return nil
}
```

- [ ] **Step 6: Serialize reconcile passes and expire leases in them**

In `data_inference.go`, in `type campaignInferenceManager struct`, add before `service *DataService`:

```go
	// ctx is the loop's context. reconcileNow passes it on, so a job started
	// during an RPC outlives that RPC.
	ctx context.Context
	// reconcileMu serializes reconcile passes. Without it, a pass that read the
	// plans before CampaignRemove deleted one could restart the job that
	// CampaignRemove had just stopped. Lock order: reconcileMu, then
	// deploymentMu.
	reconcileMu sync.Mutex
```

In `StartCampaignInference`, change the manager literal to set `ctx: ctx` (the line becomes `manager := &campaignInferenceManager{ctx: ctx, service: s, factory: factory, sender: sender, wake: make(chan struct{}, 1), jobs: map[string]*campaignInferenceJob{}}`), and after `s.inference = manager // Configured once, before registering the RPC server.` add:

```go
	// Deadlines live in memory, so every leased plan from before a restart has
	// none and is deleted here, before any job can start for it (spec §5.3).
	s.expireLeases()
```

At the top of `reconcile`, before `campaigns, err := m.service.manager.Campaigns()`, add:

```go
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	m.service.expireLeases()
```

After `reconcile`, add:

```go
// reconcileNow runs one reconcile pass outside the loop's schedule. It returns
// after the pass has stopped every job whose plan is gone.
func (m *campaignInferenceManager) reconcileNow() { m.reconcile(m.ctx) }
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `cd go && go test ./internal/agent/data/ -run RemoveCampaign -v`
Expected: PASS.

Run: `cd go && go test ./internal/agent/services/ -run 'Lease|Renew|Expiry|Remove|Restart' -race -count=3 -v`
Expected: PASS three times, no race reports, no test exceeding a few seconds.

Run: `cd go && go test ./internal/agent/services/ ./internal/agent/data/`
Expected: `ok` for both.

- [ ] **Step 8: Commit**

```bash
git add go/internal/agent/data/campaign.go go/internal/agent/data/campaign_lease_test.go go/internal/agent/services/data_campaign_lease.go go/internal/agent/services/data_campaign_lease_test.go go/internal/agent/services/data_inference.go
git commit -m "feat(agent): expire, remove and sweep leased campaigns

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M9Sy7Q2qsNmQy7v8wtkdUD"
```

---

### Task 5: Notify-only behaviour for leased campaigns

**Files:**
- Modify: `go/internal/agent/data/device_events.go` (add `RecordDeviceEvent` after `appendDeviceEvent`)
- Modify: `go/internal/agent/data/campaign_lease_test.go`
- Modify: `go/internal/agent/services/data_inference.go` (`run`, `stream`, `enqueueNotification`, `triggerInference`)
- Modify: `go/internal/agent/services/data_service.go` (`CampaignTrigger`)
- Modify: `go/internal/agent/services/data_campaign_lease_test.go`
- Modify: `go/internal/cli/assets/docs/clients/wendy-cli/commands/data.md`

**Interfaces:**
- Consumes: `Campaign.Leased()` (Task 1); `detectionNotification(campaign, source, detections)`, `data.NotificationDetection` (Task 2); `newLeaseTest`, `deployLeased` (Task 3).
- Produces: `func (m *Manager) RecordDeviceEvent(appID string, record ApplicationRecord) error`; `func (s *DataService) inferenceCurrentLocked(ctx context.Context, campaign data.Campaign) (bool, error)`; `func (s *DataService) detectionAccepted(ctx context.Context, campaign data.Campaign) bool`; `func (j *campaignInferenceJob) observeLeased(ctx context.Context, state *inferencePresence, sourceID string, detections []inference.Detection)`.

- [ ] **Step 1: Write the failing data test**

Append to `go/internal/agent/data/campaign_lease_test.go`:

```go
// A leased campaign's event goes to the device-event journal only: not into an
// open episode, and not into the pre-roll ring, from which another campaign's
// next episode would pick it up.
func TestRecordDeviceEventSkipsEpisodesAndPreRoll(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(StartOptions{Sources: []string{"applications"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = manager.Stop(AdHocEpisodeKey) })
	record := ApplicationRecord{Version: 1, Type: "event", Name: "chat-1.detected", Model: "PekingU/rtdetr_r18vd"}
	if err := manager.RecordDeviceEvent("sh.wendy.campaign.chat-1", record); err != nil {
		t.Fatal(err)
	}
	events, _, _, err := manager.DeviceEvents("sh.wendy.campaign.chat-1", "", "", true)
	if err != nil || len(events) != 1 || events[0].Name != "chat-1.detected" {
		t.Fatalf("device event not journaled: %+v %v", events, err)
	}
	current := manager.Status()
	if current == nil {
		t.Fatal("no open episode")
	}
	for _, source := range current.Sources {
		if source.Source.ID == "applications" && source.Count != 0 {
			t.Fatalf("the open episode received %d application records", source.Count)
		}
	}
	manager.mu.Lock()
	ring := len(manager.preRoll)
	manager.mu.Unlock()
	if ring != 0 {
		t.Fatalf("the pre-roll ring holds %d records", ring)
	}
}
```

- [ ] **Step 2: Write the failing services tests**

Append to `go/internal/agent/services/data_campaign_lease_test.go` (add the `inference` package import: `"github.com/wendylabsinc/wendy/go/internal/agent/inference"`):

```go
func TestLeasedDetectionNotifiesThroughJournalOnly(t *testing.T) {
	lt := newLeaseTest(t, nil)
	// Another open episode that captures applications, as an ordinary
	// campaign's would.
	if _, err := lt.service.Start(context.Background(), &agentpbv2.DataStartRequest{Sources: []string{"applications"}}); err != nil {
		t.Fatal(err)
	}
	deployLeased(t, lt.service, "chat-1", "60s")
	session := receiveInference(t, lt.factory.sessions)
	input := receiveInference(t, session.inputs)
	session.results <- inference.Result{Type: "prediction", SourceID: input.SourceID, Generation: input.Generation, Detections: []inference.Detection{
		{Label: "person", Score: .91, Box: [4]float64{1, 2, 3, 4}},
		{Label: "person", Score: .4, Box: [4]float64{5, 6, 7, 8}}, // below the 0.5 threshold
	}}

	var entries []DetectionNotification
	deadline := time.Now().Add(8 * time.Second)
	for len(entries) == 0 {
		var err error
		entries, _, _, err = lt.service.manager.Notifications("sh.wendy.campaign.chat-1", "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the detection never reached the notification journal")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(entries) != 1 || len(entries[0].Detections) != 1 || entries[0].Detections[0] != (data.NotificationDetection{Label: "person", Score: .91}) {
		t.Fatalf("journal entries: %+v", entries)
	}
	events, _, _, err := lt.service.manager.DeviceEvents("sh.wendy.campaign.chat-1", "", "", true)
	if err != nil || len(events) != 1 || events[0].Name != "chat-1.detected" {
		t.Fatalf("device events: %+v %v", events, err)
	}
	select {
	case request := <-lt.sender.requests:
		t.Fatalf("a leased notification left the device: %+v", request)
	case <-time.After(300 * time.Millisecond):
	}
	if keys := lt.service.manager.ActiveEpisodeKeys(); len(keys) != 1 || keys[0] != data.AdHocEpisodeKey {
		t.Fatalf("a leased detection opened an episode: %v", keys)
	}
	current := lt.service.manager.Status()
	for _, source := range current.Sources {
		if source.Source.ID == "applications" && source.Count != 0 {
			t.Fatalf("the other episode received %d of the leased campaign's records", source.Count)
		}
	}
	if current.ModelIO.SamplesDelivered != 0 || current.ModelIO.Predictions != 0 {
		t.Fatalf("the other episode's model ledger holds the leased campaign's frames: %+v", current.ModelIO)
	}
}

func TestLeasedCampaignCannotBeTriggered(t *testing.T) {
	lt := newLeaseTest(t, nil)
	deployLeased(t, lt.service, "chat-1", "60s")
	if _, err := lt.service.CampaignTrigger(context.Background(), &agentpbv2.DataCampaignTriggerRequest{Name: "chat-1"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("triggering a leased campaign: %v, want FailedPrecondition", err)
	}
	if keys := lt.service.manager.ActiveEpisodeKeys(); len(keys) != 0 {
		t.Fatalf("an episode opened: %v", keys)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd go && go test ./internal/agent/data/ -run RecordDeviceEvent -v`
Expected: build failure: `manager.RecordDeviceEvent undefined`.

Run: `cd go && go test ./internal/agent/services/ -run 'LeasedDetection|LeasedCampaignCannot' -v`
Expected: FAIL. `TestLeasedDetectionNotifiesThroughJournalOnly` fails on an opened episode, a received sender request or a non-zero count; `TestLeasedCampaignCannotBeTriggered` fails because the trigger succeeds.

- [ ] **Step 4: Add `RecordDeviceEvent`**

In `device_events.go`, after `appendDeviceEvent`, add:

```go
// RecordDeviceEvent appends an event to the device-event journal and nowhere
// else. Leased campaigns use it: they are notify-only, so their records reach
// no open episode and no pre-roll ring, where another campaign's next episode
// would pick them up.
func (m *Manager) RecordDeviceEvent(appID string, record ApplicationRecord) error {
	receipt, err := readBootTime()
	if err != nil {
		return err
	}
	return m.appendDeviceEvent(appID, record, bootID(), receipt)
}
```

- [ ] **Step 5: Add the notify-only path to the inference job**

In `data_inference.go`:

(a) Replace the body of `triggerInference` up to and including its first `return false, err` block with a shared check. The function becomes:

```go
func (s *DataService) triggerInference(ctx context.Context, campaign data.Campaign, record data.ApplicationRecord) (bool, error) {
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	if current, err := s.inferenceCurrentLocked(ctx, campaign); !current {
		return false, err
	}
	if _, err := s.manager.RecordCampaignApplication(campaignAppPrefix+campaign.Name, record); err != nil {
		return true, err
	}
	if _, active := s.manager.ActiveSession(campaign.Name); active {
		return true, nil
	}
	_, err := s.triggerCampaign(ctx, campaign, "event:"+campaign.Inference.Event, "event:"+campaign.Inference.Event)
	return true, err
}

// inferenceCurrentLocked reports whether campaign is still the deployed,
// enabled revision. Callers hold deploymentMu.
func (s *DataService) inferenceCurrentLocked(ctx context.Context, campaign data.Campaign) (bool, error) {
	current, err := s.manager.Campaign(campaign.Name)
	if err != nil || current.Revision != campaign.Revision || !current.Inference.IsEnabled() || ctx.Err() != nil {
		return false, err
	}
	return true, nil
}

// detectionAccepted is triggerInference's check without the episode: a leased
// campaign's detection notifies only while its plan is current (spec §5.4).
func (s *DataService) detectionAccepted(ctx context.Context, campaign data.Campaign) bool {
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	current, _ := s.inferenceCurrentLocked(ctx, campaign)
	return current
}
```

(b) In `run`, directly after `j.sourceState(result.SourceID, "detecting")`, insert:

```go
			if j.campaign.Leased() {
				j.observeLeased(ctx, presence[result.SourceID], result.SourceID, detections)
				continue
			}
```

(c) After `run`, add:

```go
// observeLeased handles a leased campaign's prediction. It writes no
// application record, so nothing reaches any episode; an arrival writes a
// device event and a journal-only notification (spec §5.4).
func (j *campaignInferenceJob) observeLeased(ctx context.Context, state *inferencePresence, sourceID string, detections []inference.Detection) {
	if state == nil || !state.observe(len(detections) > 0, time.Now(), j.campaign.Inference) {
		return
	}
	if !j.owner.service.detectionAccepted(ctx, j.campaign) {
		return
	}
	record := data.ApplicationRecord{Version: 1, Type: "event", Name: j.campaign.Inference.Event, Model: j.campaign.Inference.Model,
		Attributes: map[string]any{"campaign": j.campaign.Name, "source_id": sourceID, "model_version": j.campaign.Inference.Revision, "detections": detections}}
	if err := j.owner.service.manager.RecordDeviceEvent(campaignAppPrefix+j.campaign.Name, record); err != nil {
		j.owner.service.manager.Warnf("recording campaign %q event: %v", j.campaign.Name, err)
	}
	j.enqueueNotification(detectionNotification(j.campaign, sourceID, detections))
}
```

(d) In `stream`, directly before `if err := j.owner.service.manager.RecordModelInput(data.ModelInput{`, insert:

```go
				// A leased campaign records nothing, so its frames enter no
				// episode's model-input ledger.
				if j.campaign.Leased() {
					continue
				}
```

(e) In `enqueueNotification`, after the `if err := j.owner.service.manager.RecordNotification(request); err != nil { … }` block, insert:

```go
	// A leased campaign's notifications stay on the device: its client reads
	// the journal, and nothing goes to Cloud or a webhook.
	if j.campaign.Leased() {
		return
	}
```

- [ ] **Step 6: Refuse manual triggers of leased campaigns**

In `data_service.go`, in `CampaignTrigger`, after the `if err != nil { return nil, dataStatusError(err) }` that follows `s.manager.Campaign(req.GetName())`, add:

```go
	if campaign.Leased() {
		return nil, status.Errorf(codes.FailedPrecondition, "campaign %q is leased and notify-only; it records no episodes", campaign.Name)
	}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `cd go && go test ./internal/agent/data/ -run RecordDeviceEvent -v`
Expected: PASS.

Run: `cd go && go test ./internal/agent/services/ -run 'Lease|Leased|Renew|Expiry|Remove|Restart' -race -count=3 -v`
Expected: PASS three times, no race reports.

Run: `cd go && go test ./internal/agent/data/ ./internal/agent/services/`
Expected: `ok` for both (ordinary campaigns behave as before).

- [ ] **Step 8: Document leased campaigns**

In `data.md`, in the `## Campaign YAML reference` table:
- add a row after `fleet`: `| \`lease\` | no | Makes the campaign leased and notify-only; a duration from \`15s\` through \`10m\`. See [Leased campaigns](#leased-campaigns). |`
- change the `Required` cell of `capture`, `upload` and `export` from `yes` to `yes, except leased`.

Then insert this section directly before the `## Playing back camera capture` heading:

````markdown
### Leased campaigns

A campaign with a top-level `lease` is a *leased campaign*: a notify-only
detector that a client keeps alive by renewing it, for as long as the client
runs. A leased campaign:

- takes `lease`, a duration from `15s` through `10m`;
- selects cameras only, with no per-source `capture` or `calibration_revision`;
- needs an enabled `inference` block and `notify.on: detection`, without
  `notify.webhook`;
- takes no `capture`, `upload`, `retention`, `export`, `models` or `privacy`.

It runs inference and, on each arrival, writes a device event and a detection
notification to the device's notification journal. It records nothing: it opens
no episodes, its predictions and frames enter no other campaign's episode, and
nothing is sent to Cloud or a webhook. `wendy data campaign trigger` refuses it.

Deploying a leased campaign starts its lease, and redeploying it restarts the
lease. A redeploy cannot add or remove a lease. Clients renew and remove leased
campaigns through the `CampaignRenew` and `CampaignRemove` agent RPCs; there is
no CLI command for them. Within 5 seconds after a lease lapses, the agent stops
the campaign's inference, releases its camera and deletes the plan. Deadlines
are kept in memory, so an agent restart deletes every leased campaign.

```yaml
version: 1
name: chat-3fa91c0e-1
lease: 60s
sources:
  - camera: v4l2:/dev/video0
inference:
  model: ustc-community/dfine-nano-coco
  revision: 066438d3d8f0da137a37b38fdf3368fd4afceced
  labels: [person]
  threshold: 0.5
  rate: 2
  event: chat-3fa91c0e-1.detected
  clear_after: 5s
  cooldown: 30s
notify:
  on: detection
```

````

- [ ] **Step 9: Commit**

```bash
git add go/internal/agent/data/device_events.go go/internal/agent/data/campaign_lease_test.go go/internal/agent/services/data_inference.go go/internal/agent/services/data_service.go go/internal/agent/services/data_campaign_lease_test.go go/internal/cli/assets/docs/clients/wendy-cli/commands/data.md
git commit -m "feat(agent): run leased campaigns notify-only

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M9Sy7Q2qsNmQy7v8wtkdUD"
```

---

### Task 6: Worker resilience

**Files:**
- Modify: `go/internal/agent/inference/worker.py` (`run`, plus a new `detection_error`)
- Modify: `go/internal/agent/inference/test_worker.py`

**Interfaces:**
- Produces: `detection_error(exc) -> str` (at most 512 UTF-8 bytes, `"inference: <Type>: <message>"`); the worker's `source_error` result for a detector exception: `{"type": "source_error", "source_id", "generation", "error"}`. The Go side already handles `source_error` (it becomes the source's state in `inference_status`), so no Go change.

- [ ] **Step 1: Write the failing tests**

In `test_worker.py`, add before `if __name__ == "__main__":`:

```python
class DetectorFailureTests(unittest.TestCase):
    def test_failing_frame_reports_source_error_and_next_frame_is_scored(self):
        import json
        import types
        from unittest import mock
        import worker

        frames = ["bad", "good"]
        emitted = []
        finished = threading.Event()

        class FakeDecoder:
            def __init__(self, source_id, generation, encoding, initialization=b""):
                self.source_id, self.generation, self.encoding = source_id, generation, encoding
                self.stream = types.SimpleNamespace(stopped=False, feed=lambda payload: True)

            def take(self, now, interval):
                return frames.pop(0) if frames else None

            def stop(self):
                self.stream.stopped = True

        class Input:
            lines = [json.dumps({"source_id": "camera", "generation": 1, "encoding": "h264",
                                 "payload": "eA=="}).encode() + b"\n"]

            def readline(self, limit):
                if self.lines:
                    return self.lines.pop(0)
                finished.wait(5)
                return b""

        def detector(frame):
            if frame == "bad":
                raise ValueError("bad frame")
            return []

        def emit(result):
            emitted.append(result)
            if len(emitted) == 2:
                finished.set()

        with mock.patch.object(worker, "Decoder", FakeDecoder), \
                mock.patch.object(worker, "emit", emit), \
                mock.patch.object(worker.sys, "stdin", types.SimpleNamespace(buffer=Input())):
            worker.run({"rate": 1000}, detector)
        self.assertEqual(emitted, [
            {"type": "source_error", "source_id": "camera", "generation": 1,
             "error": "inference: ValueError: bad frame"},
            {"type": "prediction", "source_id": "camera", "generation": 1, "detections": []},
        ])

    def test_detection_error_is_cut_to_512_utf8_bytes(self):
        from worker import detection_error
        message = detection_error(RuntimeError("é" * 600))
        self.assertLessEqual(len(message.encode()), 512)
        self.assertTrue(message.startswith("inference: RuntimeError: é"))
        self.assertEqual(detection_error(KeyError()), "inference: KeyError")
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd go/internal/agent/inference && python3 -m unittest test_worker -v`
Expected: the first new test errors with `ValueError: bad frame` raised out of `worker.run`; the second fails with `ImportError: cannot import name 'detection_error'`.

- [ ] **Step 3: Implement**

In `worker.py`, after the `emit` function and `output_lock`, add:

```python
def detection_error(exc):
    """A detector failure for one frame, cut to 512 UTF-8 bytes."""
    message = "inference: " + type(exc).__name__
    if str(exc):
        message += ": " + str(exc)
    return message.encode()[:512].decode(errors="ignore")
```

In `run`, replace

```python
                detections = detector(frame)
```

with

```python
                try:
                    detections = detector(frame)
                except Exception as exc:
                    # One bad frame costs that frame, not the worker. Before,
                    # the exception ended the process, and the agent lost the
                    # stderr tail once the model had loaded.
                    emit({"type": "source_error", "source_id": decoder.source_id,
                          "generation": decoder.generation, "error": detection_error(exc)})
                    continue
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd go/internal/agent/inference && python3 -m unittest test_worker -v`
Expected: all tests OK (the existing four skips remain).

Run: `cd go && go test ./internal/agent/inference/`
Expected: `ok` (the runtime embeds `worker.py`; its hash changes, so devices sync a new runtime directory from the uv cache on first use).

- [ ] **Step 5: Commit**

```bash
git add go/internal/agent/inference/worker.py go/internal/agent/inference/test_worker.py
git commit -m "fix(agent): keep the inference worker serving after a detector exception

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M9Sy7Q2qsNmQy7v8wtkdUD"
```

---

### Task 7: Verify, check on the Orin Nano, open the PR

**Files:** none changed unless a check fails. Scratch files go in the session scratchpad (`$SCRATCH` below), never in the repo.

- [ ] **Step 1: Full local verification**

```bash
cd go
gofmt -l .
go vet ./internal/agent/...
go test ./internal/agent/data/ ./internal/agent/services/ ./internal/agent/inference/
go test -race ./internal/agent/services/ -run 'Lease|Leased|Renew|Expiry|Remove|Restart|Inference'
go build ./...
GOOS=linux GOARCH=arm64 go build -o /dev/null ./cmd/wendy-agent/
cd internal/agent/inference && python3 -m unittest test_worker
```

Expected: `gofmt -l` prints nothing; every other command succeeds. `GOOS=linux go build ./...` is known to fail on `main` (cgo `gousb` in the CLI), so only the agent is cross-built.

- [ ] **Step 2: Side-load the agent onto the Orin Nano**

The Jetson Orin Nano (`hopeful-glider`, USB-C to this Mac) has the Logitech Brio 101 attached. The campaign path decodes the agent's own encoded camera stream, so the Brio needs no virtual H.264 camera here (that recipe is only for model watch, PR C). If a source state later says `unsupported camera encoding`, stop and report rather than building one.

```bash
SCRATCH=<session scratchpad>
wendy device list                        # find the selector; call it $DEV
wendy device info --device "$DEV" --json # record the agent version to restore later
cd go
GOOS=linux GOARCH=arm64 go build -o "$SCRATCH/wendy-agent" ./cmd/wendy-agent/
go build -o "$SCRATCH/wendy" ./cmd/wendy/  # the deploy command validates YAML locally, so use this branch's CLI
wendy device update --device "$DEV" --binary "$SCRATCH/wendy-agent"
"$SCRATCH/wendy" data sources --device "$DEV"   # note the Brio's camera source id
```

- [ ] **Step 3: Deploy a leased campaign and see one detection**

Write `$SCRATCH/chat-test.yaml` with `leasedTestYAML`'s shape: `name: chat-test-1`, `lease: 60s`, `camera: <the Brio's source id>`, and the default detector from spec §8 (D-FINE nano). Then:

```bash
"$SCRATCH/wendy" data campaign deploy --device "$DEV" --skip-cloud-registration "$SCRATCH/chat-test.yaml"
"$SCRATCH/wendy" data campaign inspect chat-test-1 --device "$DEV" --json   # repeat until inference_status.state is "running"
```

Record how long `loading` lasted (the first start installs the Python runtime and downloads the model, which takes minutes). Then ask Ethan to step into the Brio's view (or hold up a photo of a person) for a few seconds, and read the journals:

```bash
wendy device shell --device "$DEV" -- cat /var/lib/wendy-agent/data/episodes/.data-notifications.json
wendy device shell --device "$DEV" -- cat /var/lib/wendy-agent/data/episodes/.device-events.json
"$SCRATCH/wendy" data episodes --device "$DEV"
```

(If the agent unit sets `WENDY_DATA_DIR`, use that root instead: `wendy device shell --device "$DEV" -- systemctl show wendyos-agent --no-pager -p Environment`.)

Expected: one notification for `chat-test-1` with `detections` such as `[{"label":"person","score":0.9…}]`; one `chat-test-1.detected` device event; no new episode.

- [ ] **Step 4: Watch the lease lapse**

Do nothing for 65 s after the deploy (no renewal is possible from the CLI). Then:

```bash
"$SCRATCH/wendy" data campaign list --device "$DEV"
wendy device shell --device "$DEV" -- sh -c 'pgrep -af worker.py || echo no-worker'
wendy device shell --device "$DEV" -- journalctl -u wendyos-agent --no-pager -n 50
```

Expected: `chat-test-1` is gone, no `worker.py` process remains, and the log contains `leased campaign "chat-test-1" was removed because its lease lapsed or the agent restarted`.

- [ ] **Step 5: Restart sweep**

```bash
"$SCRATCH/wendy" data campaign deploy --device "$DEV" --skip-cloud-registration "$SCRATCH/chat-test.yaml"
wendy device shell --device "$DEV" -- systemctl restart wendyos-agent
"$SCRATCH/wendy" data campaign list --device "$DEV"   # after the agent is back
```

Expected: `chat-test-1` is absent after the restart.

- [ ] **Step 6: Restore the device**

```bash
wendy device update --device "$DEV"            # decline any OS update prompt
wendy device info --device "$DEV" --json       # version must equal the one recorded in Step 2
```

Delete `$SCRATCH/wendy-agent`, `$SCRATCH/wendy` and `$SCRATCH/chat-test.yaml`.

- [ ] **Step 7: Push and open the draft PR**

```bash
cd go && gofmt -l . && cd ..
git push -u origin ed/chat-watches-leased-campaigns
git log --format='%an <%ae>' -- go/internal/agent/inference/worker.py | sort -u   # find Joannis's GitHub login for the reviewer
gh pr create --draft --base main --title "feat(agent): leased, notify-only campaigns for chat watches (P-WDY-258 M3, PR A)" --body-file "$SCRATCH/pr-a-body.md"
gh pr edit --add-reviewer <joannis-login>
```

`$SCRATCH/pr-a-body.md` contains: a summary of §5 in five bullets; a link to the spec; the points Joannis should weigh: the lease enters the revision only when set (as `backend` and `model_file` do), `validateNotify` is moved code, `reconcileMu` and the lock order `reconcileMu` → `deploymentMu`, `detections` on every detection notification (webhook payloads gain the field), the notify-only bypass of `triggerInference`; the test commands from Step 1; the on-device results from Steps 3–5 with the first-start time; and it ends with:

```
🤖 Generated with [Claude Code](https://claude.com/claude-code)
```
