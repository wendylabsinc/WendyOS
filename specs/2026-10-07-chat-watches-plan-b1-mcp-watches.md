# Chat watches PR B1: MCP watch tools, implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `wendy mcp serve` can start, track and stop camera watches on the connected device, push their events and status to its client as MCP notifications, and serve them to clients that only poll.

**Architecture:** A per-server `watchManager` owns this session's watches (at most two), their sequence numbers, event buffers and notifications. It drives a small `watchBackend` interface. The only backend in this PR runs each watch as a leased, notify-only Wendy Data campaign (PR A, #2206). It deploys the campaign, renews its lease every 20 s, polls its inference status every 5 s, and reads the device's notification journal once a second through one shared poller. Five tools in the existing `hardware` group expose it: `watch_sources`, `watch_start`, `watch_list`, `watch_stop`, `watch_events`.

**Tech Stack:** Go 1.27, mcp-go v0.54.0 (`github.com/mark3labs/mcp-go`), gRPC `DataService` v2 client (`agentpbv2`), `go/internal/agent/data` for campaign types and local validation.

**Spec:** `specs/2026-10-05-chat-watches-design.md`, §6.1–6.3 and §6.5–6.6, §8, §9, §11 (PR B). Read §6 before starting any task. The chat half of PR B is the separate plan `specs/2026-10-07-chat-watches-plan-b2-chat-watches.md`.

**Base:** branch `ed/chat-watches-mcp` from `ed/chat-watches-leased-campaigns` (PR A, #2206, approved, not yet merged). Rebase onto `main` once #2206 merges.

## Global Constraints

- Default detector: `ustc-community/dfine-nano-coco` at revision `066438d3d8f0da137a37b38fdf3368fd4afceced`, id `dfine-nano-coco`, labels exactly the checkpoint's 80 `id2label` values in class order (spec §8, D5 as revised 2026-10-07).
- Leased campaign shape (spec §5.1): `lease: 60s`, one camera source, `inference.rate: 2`, `clear_after: 5s`, `cooldown: 30s`, `event: <name>.detected`, `notify.on: detection`, nothing else.
- Renew every 20 s; poll inference status every 5 s; poll the notification journal every 1 s, from a cursor taken before the first watch's campaign is deployed.
- Campaign names `chat-<8 hex>-<n>`; watch ids `w<n>`; at most two active watches per server.
- `watch_start` arguments: `camera` (a healthy camera source id), `classes` (1–10 of the detector's labels, unique), `min_confidence` (0.3–0.95, default 0.5), `label` (optional, at most 64 bytes, default the camera's name). It returns when READY or ERROR, or after 30 s while PREPARING.
- Status mapping: agent `pending`/`loading` → `PREPARING`; `running` → `READY`; `error` and `waiting_for_cameras` → `ERROR` (not terminal; the agent retries). Renew or inspect NOT_FOUND ends the watch with "the device restarted or the watch expired".
- Older agents: a deploy rejected because of the unknown `lease` field, an UNIMPLEMENTED renew or remove, or a notification journal the agent does not mark as such all give "This device's agent is too old for watches; update it with `wendy device update`." Nothing is left on the device.
- Notifications go to the `stdio` session: `notifications/wendy/watch_event` `{watch_id, label, sequence, kind, classes:[{label,score}], occurred_at}` and `notifications/wendy/watch_status` `{watch_id, label, state, reason, camera, watching}`. A failed send is counted and reported as `missed_notifications` on the watch's next status.
- Event buffer: the last 100 events per watch. `watch_events` `wait_seconds` 0–120.
- A connection change ends every watch with "device changed" and sends nothing to the old device. On server exit, active watches are removed concurrently within 1.5 s.
- The five tools are in the `hardware` tool group; the default (`core`) set and its count (16) do not change.
- Run `gofmt -l .` from `go/` before every push. Branch prefix `ed/`. Commits authored as `24462281+EBro912@users.noreply.github.com`.

## Deviations from the spec, decided while planning

1. **Exit cleanup is concurrent and bounded by 1.5 s**, not 5 s per call (§6.1). mcp-go's stdio client closes the server's stdin, waits 2 s, then sends SIGTERM (`client/transport/stdio.go`, `gracefulShutdownTimeout`). PR A's `CampaignRemove` deletes the plan before it waits for the job, so a short client deadline still removes the campaign. The lease covers anything the device missed.
2. **No line in the server instructions** (§6.5). They are 2039 of the 2048 bytes `instructions_test.go` allows, and may name only core tools. The `wendy://guide` resource and the tool descriptions carry the watch guidance instead.
3. **`watch_status` also carries `camera` and `watching`** (the watched classes). Chat (PR B2) needs them to write the event prompt without an extra call.
4. **`waiting_for_cameras` maps to ERROR** with "the camera is unavailable". §6.3's table does not list it; a missing camera is something the user should hear about.
5. **`watchBackend.Start` takes the connection**: `Start(ctx, conn, spec)` instead of `Start(ctx, spec)`, because the backend is shared across connection changes.

## Review Focus

1. **A detection between the first journal read and the deploy.** It must reach the watch: the poller's cursor is taken before `CampaignDeploy`. Test: Task 3, `TestCampaignPollerStartsBeforeDeploy`.
2. **The device switches while `watch_start` is deploying.** The new watch must end with "device changed", not run unseen against the old device. Test: Task 4, `TestWatchManagerEndsWatchStartedDuringConnectionChange`.
3. **Notification queue full.** Events must still be in `watch_events`, and the next status must say how many notifications were missed. Test: Task 4, `TestWatchManagerCountsFailedNotifications`.
4. **A client that reads `watch_events` long after the buffer wrapped.** It must be told it missed events (`gap: true`). Test: Task 4, `TestWatchManagerBuffersLastHundredEvents`.
5. **Two watches end at server exit when one device call hangs.** Both removals must be attempted, and exit must not wait past the deadline. Test: Task 4, `TestWatchManagerShutdownStopsConcurrently`.

---

## File map

| File | Task | Responsibility |
|---|---|---|
| `go/internal/cli/mcp/watch_detector.go`, `watch_detector_test.go`, `testdata/dfine-nano-coco-id2label.json` | 1 | the default detector entry and its label check |
| `go/internal/cli/mcp/watch_backend.go` | 2 | shared watch types and the backend interface |
| `go/internal/cli/mcp/watch_campaign.go`, `watch_campaign_test.go` | 2, 3 | the campaign backend: deploy, renew, status, stop, journal poller |
| `go/internal/cli/mcp/watch_fake_test.go` | 2, 3, 5 | fake `DataServiceClient` for watch tests |
| `go/internal/cli/mcp/watch_manager.go`, `watch_manager_test.go` | 4 | ids, cap, sequences, buffers, notifications, connection changes, shutdown |
| `go/internal/cli/mcp/watch_tools.go`, `watch_tools_test.go` | 5 | the five tools |
| `go/internal/cli/mcp/server.go`, `tool_groups.go`, `tools_guide.go` | 5 | wiring, group, guide |
| `go/internal/cli/assets/docs/integrations/mcp.mdx` | 5 | docs |

All commands run from the worktree root `/Users/ethan/Documents/WendyAgent-chat-watches` unless they `cd`. Before Task 1: `git checkout ed/chat-watches-mcp` (created from `ed/chat-watches-leased-campaigns`; it carries this plan and plan B2).

---

### Task 1: The default detector entry

**Files:**
- Create: `go/internal/cli/mcp/watch_detector.go`
- Create: `go/internal/cli/mcp/testdata/dfine-nano-coco-id2label.json`
- Create: `go/internal/cli/mcp/watch_detector_test.go`

**Interfaces:**
- Produces: `type watchDetector struct { ID, Model, Revision string; Labels []string }`; `var defaultWatchDetector watchDetector`; `func (d watchDetector) hasLabel(label string) bool`.

- [ ] **Step 1: Record the fixture**

The test compares against the checkpoint's own `config.json`, recorded once so tests never use the network:

```bash
mkdir -p go/internal/cli/mcp/testdata
curl -sfL "https://huggingface.co/ustc-community/dfine-nano-coco/resolve/066438d3d8f0da137a37b38fdf3368fd4afceced/config.json" \
  | python3 -I -c 'import json,sys; d=json.load(sys.stdin)["id2label"]; print(json.dumps({"source":"https://huggingface.co/ustc-community/dfine-nano-coco/blob/066438d3d8f0da137a37b38fdf3368fd4afceced/config.json","id2label":{str(i):d[str(i)] for i in range(len(d))}},indent=2))' \
  > go/internal/cli/mcp/testdata/dfine-nano-coco-id2label.json
python3 -I -c 'import json; d=json.load(open("go/internal/cli/mcp/testdata/dfine-nano-coco-id2label.json")); print(len(d["id2label"]), d["id2label"]["0"], d["id2label"]["79"])'
```

Expected: `80 person toothbrush`.

- [ ] **Step 2: Write the failing test**

Create `go/internal/cli/mcp/watch_detector_test.go`:

```go
package mcp

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The embedded labels must be the checkpoint's own, in class order: watch_start
// accepts exactly these names, and the agent rejects labels the model lacks.
func TestDefaultWatchDetectorLabelsMatchCheckpoint(t *testing.T) {
	raw, err := os.ReadFile("testdata/dfine-nano-coco-id2label.json")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		ID2Label map[string]string `json:"id2label"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.ID2Label) != len(defaultWatchDetector.Labels) {
		t.Fatalf("detector has %d labels, checkpoint has %d", len(defaultWatchDetector.Labels), len(config.ID2Label))
	}
	for i, label := range defaultWatchDetector.Labels {
		if want := config.ID2Label[strconv.Itoa(i)]; label != want {
			t.Fatalf("label %d = %q, checkpoint says %q", i, label, want)
		}
	}
	if !defaultWatchDetector.hasLabel("motorbike") || defaultWatchDetector.hasLabel("motorcycle") {
		t.Fatal("labels must be the checkpoint's spelling, not COCO's")
	}
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(defaultWatchDetector.Revision) || defaultWatchDetector.Model != "ustc-community/dfine-nano-coco" {
		t.Fatalf("detector is not pinned: %+v", defaultWatchDetector)
	}
}
```

- [ ] **Step 3: Run it to verify it fails**

Run: `cd go && go test ./internal/cli/mcp/ -run TestDefaultWatchDetector -v`
Expected: build failure, `undefined: defaultWatchDetector`.

- [ ] **Step 4: Implement**

Create `go/internal/cli/mcp/watch_detector.go`:

```go
package mcp

import "slices"

// watchDetector is the detector chat watches run (design §8, D5): a model run
// as its publisher released it, pinned to a commit. Neither the LLM nor the
// user can choose another model or a URL through the watch tools. Changing the
// default detector means changing this one entry.
type watchDetector struct {
	ID       string
	Model    string
	Revision string
	// Labels are the checkpoint's class names in class order, copied from its
	// config.json at Revision. A watch's classes must be among them.
	Labels []string
}

var defaultWatchDetector = watchDetector{
	ID:       "dfine-nano-coco",
	Model:    "ustc-community/dfine-nano-coco",
	Revision: "066438d3d8f0da137a37b38fdf3368fd4afceced",
	Labels: []string{
		"person", "bicycle", "car", "motorbike", "aeroplane", "bus", "train", "truck", "boat",
		"traffic light", "fire hydrant", "stop sign", "parking meter", "bench", "bird", "cat", "dog",
		"horse", "sheep", "cow", "elephant", "bear", "zebra", "giraffe", "backpack", "umbrella",
		"handbag", "tie", "suitcase", "frisbee", "skis", "snowboard", "sports ball", "kite",
		"baseball bat", "baseball glove", "skateboard", "surfboard", "tennis racket", "bottle",
		"wine glass", "cup", "fork", "knife", "spoon", "bowl", "banana", "apple", "sandwich", "orange",
		"broccoli", "carrot", "hot dog", "pizza", "donut", "cake", "chair", "sofa", "pottedplant", "bed",
		"diningtable", "toilet", "tvmonitor", "laptop", "mouse", "remote", "keyboard", "cell phone",
		"microwave", "oven", "toaster", "sink", "refrigerator", "book", "clock", "vase", "scissors",
		"teddy bear", "hair drier", "toothbrush",
	},
}

func (d watchDetector) hasLabel(label string) bool { return slices.Contains(d.Labels, label) }
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `cd go && go test ./internal/cli/mcp/ -run TestDefaultWatchDetector -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go/internal/cli/mcp/watch_detector.go go/internal/cli/mcp/watch_detector_test.go go/internal/cli/mcp/testdata/dfine-nano-coco-id2label.json
git commit -m "feat(mcp): pin the default watch detector

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M9Sy7Q2qsNmQy7v8wtkdUD"
```

---

### Task 2: Watch types and the campaign backend's per-watch lifecycle

**Files:**
- Create: `go/internal/cli/mcp/watch_backend.go`
- Create: `go/internal/cli/mcp/watch_campaign.go`
- Create: `go/internal/cli/mcp/watch_fake_test.go`
- Create: `go/internal/cli/mcp/watch_campaign_test.go`

**Interfaces:**
- Consumes: `defaultWatchDetector`, `watchDetector` (Task 1).
- Produces (all in package `mcp`):
  - `type watchState string` with `watchPreparing = "PREPARING"`, `watchReady = "READY"`, `watchError = "ERROR"`, `watchEnded = "ENDED"`.
  - `type watchSpec struct { Name, CameraID, CameraName string; Classes []string; MinConfidence float64; Label string }` (`Name` is the campaign name the manager assigns).
  - `type watchClass struct { Label string `json:"label"`; Score float64 `json:"score"` }`.
  - `type watchUpdate struct { Event *watchEventUpdate; Status *watchStatusUpdate; Gap string }`, `type watchEventUpdate struct { Kind string; Classes []watchClass; OccurredAt time.Time }`, `type watchStatusUpdate struct { State watchState; Reason string }`.
  - `type watchBackend interface { Start(ctx context.Context, conn *grpcclient.AgentConnection, spec watchSpec) (watchHandle, error) }`, `type watchHandle interface { Updates() <-chan watchUpdate; Stop(ctx context.Context) error }`.
  - `var errWatchAgentTooOld error`; `func newCampaignWatchBackend(detector watchDetector) *campaignWatchBackend` with fields `renewEvery`, `statusEvery`, `pollEvery time.Duration` that tests may shorten before the first `Start`.
  - Test fake `watchDataClient` (in `watch_fake_test.go`) and helper `newWatchDataClient()`.

- [ ] **Step 1: Write the shared types**

Create `go/internal/cli/mcp/watch_backend.go`:

```go
package mcp

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

// watchState is a watch's state as clients see it (design §6.3).
type watchState string

const (
	watchPreparing watchState = "PREPARING"
	watchReady     watchState = "READY"
	watchError     watchState = "ERROR" // not terminal: the device keeps retrying
	watchEnded     watchState = "ENDED"
)

// watchSpec is what a watch asked for. Name is the device-side identity the
// manager assigns (a campaign name for the campaign backend).
type watchSpec struct {
	Name          string
	CameraID      string
	CameraName    string
	Classes       []string
	MinConfidence float64
	Label         string
}

// watchClass is one detected class and its score.
type watchClass struct {
	Label string  `json:"label"`
	Score float64 `json:"score"`
}

// watchUpdate is one thing a backend reports, in order: an event, a status
// change, or a gap (a stretch of detections that may have been missed).
type watchUpdate struct {
	Event  *watchEventUpdate
	Status *watchStatusUpdate
	Gap    string
}

type watchEventUpdate struct {
	Kind       string // "entered" or "left"
	Classes    []watchClass
	OccurredAt time.Time
}

type watchStatusUpdate struct {
	State  watchState
	Reason string
}

// watchBackend runs watches on a device. Backends only translate; the manager
// assigns sequence numbers and owns buffering and notifications (design §6.2).
type watchBackend interface {
	// Start creates the watch on the device and returns once it exists there.
	// Readiness and events arrive on the handle.
	Start(ctx context.Context, conn *grpcclient.AgentConnection, spec watchSpec) (watchHandle, error)
}

// watchHandle is one running watch. Updates is closed after Stop, or after the
// backend reports the watch ENDED by itself.
type watchHandle interface {
	Updates() <-chan watchUpdate
	Stop(ctx context.Context) error
}
```

- [ ] **Step 2: Write the fake DataService client**

Create `go/internal/cli/mcp/watch_fake_test.go`:

```go
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
	mu         sync.Mutex
	calls      []string
	deployed   map[string]data.Campaign
	inference  map[string]data.InferenceStatus // default: running
	renews     map[string]int
	removed    []string
	deployErr  error
	renewErr   error
	inspectErr error
	removeErr  error
	removeWait time.Duration
	journal    []data.CampaignNotification
	gapOnce    bool
	oldJournal bool // Events does not mark the response as the notification journal
	sources    []*agentpbv2.DataSource
}

func newWatchDataClient() *watchDataClient {
	return &watchDataClient{
		deployed: map[string]data.Campaign{}, inference: map[string]data.InferenceStatus{}, renews: map[string]int{},
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
```

- [ ] **Step 3: Write the failing tests**

Create `go/internal/cli/mcp/watch_campaign_test.go`:

```go
package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func fastCampaignBackend() *campaignWatchBackend {
	b := newCampaignWatchBackend(defaultWatchDetector)
	b.renewEvery, b.statusEvery, b.pollEvery = 20*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond
	return b
}

func testWatchSpec(name string) watchSpec {
	return watchSpec{Name: name, CameraID: "v4l2:/dev/video0", CameraName: "Brio 101", Classes: []string{"person", "dog"}, MinConfidence: 0.6, Label: "front door"}
}

// nextUpdate waits for the next update on h, failing after two seconds.
func nextUpdate(t *testing.T, h watchHandle) watchUpdate {
	t.Helper()
	select {
	case u, ok := <-h.Updates():
		if !ok {
			t.Fatal("updates closed")
		}
		return u
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a watch update")
		return watchUpdate{}
	}
}

// nextStatus skips events and gaps until a status arrives.
func nextStatus(t *testing.T, h watchHandle) watchStatusUpdate {
	t.Helper()
	for {
		if u := nextUpdate(t, h); u.Status != nil {
			return *u.Status
		}
	}
}

func TestCampaignWatchDeploysLeasedNotifyOnlyCampaign(t *testing.T) {
	client := newWatchDataClient()
	b := fastCampaignBackend()
	h, err := b.Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop(context.Background())
	c, ok := client.campaign("chat-0a1b2c3d-1")
	if !ok {
		t.Fatal("campaign was not deployed")
	}
	i := c.Inference
	if c.Lease != "60s" || !c.Leased() || len(c.Sources) != 1 || c.Sources[0].Camera != "v4l2:/dev/video0" ||
		i.Model != defaultWatchDetector.Model || i.Revision != defaultWatchDetector.Revision || i.Threshold != 0.6 || i.Rate != 2 ||
		len(i.Labels) != 2 || i.Labels[0] != "person" || i.Event != "chat-0a1b2c3d-1.detected" || i.ClearAfter != "5s" || i.Cooldown != "30s" ||
		c.Notify == nil || c.Notify.On != data.NotifyOnDetection || c.Notify.Webhook != "" {
		t.Fatalf("not the leased notify-only shape: %+v %+v", c, i)
	}
}

func TestCampaignWatchOldAgentRefusesCleanly(t *testing.T) {
	for name, deployErr := range map[string]error{
		"unknown lease field": status.Error(codes.InvalidArgument, "parsing campaign YAML: yaml: unmarshal errors:\n  line 3: field lease not found in type data.Campaign"),
		"unimplemented":       status.Error(codes.Unimplemented, "unknown method"),
	} {
		t.Run(name, func(t *testing.T) {
			client := newWatchDataClient()
			client.deployErr = deployErr
			_, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
			if !errors.Is(err, errWatchAgentTooOld) {
				t.Fatalf("got %v, want errWatchAgentTooOld", err)
			}
		})
	}
	client := newWatchDataClient()
	client.deployErr = status.Error(codes.InvalidArgument, "no healthy camera matches")
	if _, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1")); err == nil || errors.Is(err, errWatchAgentTooOld) {
		t.Fatalf("an ordinary refusal was reported as an old agent: %v", err)
	}
	if _, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{}, testWatchSpec("chat-0a1b2c3d-1")); !errors.Is(err, errWatchAgentTooOld) {
		t.Fatalf("a connection without DataService: %v", err)
	}
}

func TestCampaignWatchMapsInferenceStatus(t *testing.T) {
	client := newWatchDataClient()
	client.setInference("chat-0a1b2c3d-1", data.InferenceStatus{State: "loading"})
	h, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop(context.Background())
	if s := nextStatus(t, h); s.State != watchPreparing {
		t.Fatalf("loading -> %+v", s)
	}
	client.setInference("chat-0a1b2c3d-1", data.InferenceStatus{State: "running"})
	if s := nextStatus(t, h); s.State != watchReady {
		t.Fatalf("running -> %+v", s)
	}
	client.setInference("chat-0a1b2c3d-1", data.InferenceStatus{State: "error", Error: "model download failed"})
	if s := nextStatus(t, h); s.State != watchError || s.Reason != "model download failed" {
		t.Fatalf("error -> %+v", s)
	}
	client.setInference("chat-0a1b2c3d-1", data.InferenceStatus{State: "waiting_for_cameras"})
	if s := nextStatus(t, h); s.State != watchError || s.Reason != "the camera is unavailable" {
		t.Fatalf("waiting_for_cameras -> %+v", s)
	}
}

func TestCampaignWatchRenewsAndEndsWhenTheLeaseIsGone(t *testing.T) {
	client := newWatchDataClient()
	h, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		client.mu.Lock()
		renews := client.renews["chat-0a1b2c3d-1"]
		client.mu.Unlock()
		if renews >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the lease was not renewed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	client.mu.Lock()
	delete(client.deployed, "chat-0a1b2c3d-1") // the agent restarted
	client.mu.Unlock()
	for {
		u, ok := <-h.Updates()
		if !ok {
			t.Fatal("updates closed without an ENDED status")
		}
		if u.Status != nil && u.Status.State == watchEnded {
			if u.Status.Reason != "the device restarted or the watch expired" {
				t.Fatalf("reason %q", u.Status.Reason)
			}
			break
		}
	}
	if _, ok := <-h.Updates(); ok {
		t.Fatal("updates stayed open after the watch ended")
	}
}

func TestCampaignWatchStopRemovesTheCampaign(t *testing.T) {
	client := newWatchDataClient()
	h, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := client.campaign("chat-0a1b2c3d-1"); ok {
		t.Fatal("campaign still deployed after Stop")
	}
	for range h.Updates() {
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("a second Stop must succeed (the campaign is already gone): %v", err)
	}
	client.removeErr = status.Error(codes.Unimplemented, "unknown method")
	h2, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h2.Stop(context.Background()); !errors.Is(err, errWatchAgentTooOld) {
		t.Fatalf("an agent without CampaignRemove: %v", err)
	}
}
```

- [ ] **Step 4: Run the tests to verify they fail**

Run: `cd go && go test ./internal/cli/mcp/ -run 'CampaignWatch' -v`
Expected: build failure, `undefined: newCampaignWatchBackend`.

- [ ] **Step 5: Implement the backend's per-watch lifecycle**

Create `go/internal/cli/mcp/watch_campaign.go`:

```go
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
)

const (
	// A 60 s lease renewed every 20 s survives two missed renewals (design §6.3).
	watchLease         = "60s"
	watchRenewEvery    = 20 * time.Second
	watchStatusEvery   = 5 * time.Second
	watchPollEvery     = time.Second
	watchInferenceRate = 2
	watchCallTimeout   = 10 * time.Second
	watchGoneReason    = "the device restarted or the watch expired"
)

// errWatchAgentTooOld is returned for agents without leased campaigns. Their
// campaign parser rejects the unknown lease field and they lack the renew and
// remove RPCs, so a refused watch leaves nothing on the device (design §5.7).
var errWatchAgentTooOld = errors.New("This device's agent is too old for watches; update it with `wendy device update`.")

// campaignWatchBackend runs each watch as a leased, notify-only Wendy Data
// campaign (design §6.3).
type campaignWatchBackend struct {
	detector    watchDetector
	renewEvery  time.Duration
	statusEvery time.Duration
	pollEvery   time.Duration

	mu      sync.Mutex
	watches map[string]*campaignWatch // by campaign name
}

func newCampaignWatchBackend(detector watchDetector) *campaignWatchBackend {
	return &campaignWatchBackend{detector: detector, renewEvery: watchRenewEvery, statusEvery: watchStatusEvery, pollEvery: watchPollEvery, watches: map[string]*campaignWatch{}}
}

func (b *campaignWatchBackend) Start(ctx context.Context, conn *grpcclient.AgentConnection, spec watchSpec) (watchHandle, error) {
	if conn == nil || conn.DataService == nil {
		return nil, errWatchAgentTooOld
	}
	plan, err := b.campaignYAML(spec)
	if err != nil {
		return nil, err
	}
	client := conn.DataService
	if _, err := client.CampaignDeploy(ctx, &agentpbv2.DataCampaignDeployRequest{CampaignYaml: plan}); err != nil {
		return nil, watchDeviceError(err)
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	w := &campaignWatch{backend: b, client: client, name: spec.Name, ctx: watchCtx, cancel: cancel, updates: make(chan watchUpdate, 32), done: make(chan struct{})}
	b.mu.Lock()
	b.watches[spec.Name] = w
	b.mu.Unlock()
	go w.run()
	return w, nil
}

// campaignYAML builds the leased campaign of design §5.1 and validates it
// locally, so an invalid plan never reaches the device.
func (b *campaignWatchBackend) campaignYAML(spec watchSpec) ([]byte, error) {
	campaign := data.Campaign{
		Version: data.CampaignVersion, Name: spec.Name, Lease: watchLease,
		Sources: []data.CampaignSource{{Camera: spec.CameraID}},
		Inference: &data.CampaignInference{
			Model: b.detector.Model, Revision: b.detector.Revision, Labels: spec.Classes,
			Threshold: spec.MinConfidence, Rate: watchInferenceRate, Event: spec.Name + ".detected",
			ClearAfter: "5s", Cooldown: "30s",
		},
		Notify: &data.CampaignNotify{On: data.NotifyOnDetection},
	}
	plan, err := yaml.Marshal(campaign)
	if err != nil {
		return nil, err
	}
	if _, err := data.ParseCampaign(plan); err != nil {
		return nil, fmt.Errorf("building the watch campaign: %w", err)
	}
	return plan, nil
}

// watchDeviceError maps a device refusal. An agent that predates leased
// campaigns parses plans with unknown fields disallowed, so it names lease.
func watchDeviceError(err error) error {
	st, ok := status.FromError(err)
	if ok && (st.Code() == codes.Unimplemented || st.Code() == codes.InvalidArgument && strings.Contains(st.Message(), "field lease not found")) {
		return errWatchAgentTooOld
	}
	return err
}

func (b *campaignWatchBackend) forget(w *campaignWatch) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.watches[w.name] == w {
		delete(b.watches, w.name)
	}
}

// campaignWatch renews one campaign's lease and follows its inference state.
type campaignWatch struct {
	backend *campaignWatchBackend
	client  agentpbv2.DataServiceClient
	name    string
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}

	sendMu  sync.Mutex
	closed  bool
	updates chan watchUpdate

	lastState  watchState
	lastReason string
}

func (w *campaignWatch) Updates() <-chan watchUpdate { return w.updates }

// send delivers u unless the watch has stopped. The manager drains updates
// until they close, so a send blocks only briefly.
func (w *campaignWatch) send(u watchUpdate) bool {
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	if w.closed {
		return false
	}
	select {
	case w.updates <- u:
		return true
	case <-w.ctx.Done():
		return false
	}
}

func (w *campaignWatch) closeUpdates() {
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	if !w.closed {
		w.closed = true
		close(w.updates)
	}
}

func (w *campaignWatch) run() {
	defer close(w.done)
	defer w.closeUpdates()
	defer w.backend.forget(w)
	renew := time.NewTicker(w.backend.renewEvery)
	defer renew.Stop()
	inspect := time.NewTicker(w.backend.statusEvery)
	defer inspect.Stop()
	if !w.refreshStatus() {
		return
	}
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-renew.C:
			if !w.renew() {
				return
			}
		case <-inspect.C:
			if !w.refreshStatus() {
				return
			}
		}
	}
}

// renew pushes the lease forward. A transient failure is retried on the next
// tick. NOT_FOUND means the device restarted or the lease lapsed.
func (w *campaignWatch) renew() bool {
	ctx, cancel := context.WithTimeout(w.ctx, watchCallTimeout)
	defer cancel()
	_, err := w.client.CampaignRenew(ctx, &agentpbv2.DataCampaignRenewRequest{Name: w.name})
	switch status.Code(err) {
	case codes.OK:
		return true
	case codes.NotFound:
		w.end(watchGoneReason)
		return false
	case codes.Unimplemented:
		w.end(errWatchAgentTooOld.Error())
		return false
	}
	return w.ctx.Err() == nil
}

// refreshStatus reports a change in the campaign's inference state.
func (w *campaignWatch) refreshStatus() bool {
	ctx, cancel := context.WithTimeout(w.ctx, watchCallTimeout)
	defer cancel()
	campaign, err := w.client.CampaignInspect(ctx, &agentpbv2.DataCampaignInspectRequest{Name: w.name})
	if status.Code(err) == codes.NotFound {
		w.end(watchGoneReason)
		return false
	}
	if err != nil {
		return w.ctx.Err() == nil
	}
	var plan data.Campaign
	if json.Unmarshal(campaign.GetPlanJson(), &plan) != nil || plan.InferenceStatus == nil {
		return true
	}
	state, reason := watchStateFromInference(*plan.InferenceStatus)
	if state != w.lastState || reason != w.lastReason {
		w.lastState, w.lastReason = state, reason
		return w.send(watchUpdate{Status: &watchStatusUpdate{State: state, Reason: reason}})
	}
	return true
}

func (w *campaignWatch) end(reason string) {
	w.send(watchUpdate{Status: &watchStatusUpdate{State: watchEnded, Reason: reason}})
}

// watchStateFromInference maps the agent's inference state (design §6.3).
func watchStateFromInference(s data.InferenceStatus) (watchState, string) {
	switch s.State {
	case "running":
		return watchReady, ""
	case "error":
		return watchError, s.Error
	case "waiting_for_cameras":
		if s.Error != "" {
			return watchError, "the camera is unavailable: " + s.Error
		}
		return watchError, "the camera is unavailable"
	case "loading":
		return watchPreparing, "loading the detector; the first watch on a device installs it first, which takes a few minutes"
	}
	return watchPreparing, ""
}

// Stop ends renewals, then removes the campaign. NOT_FOUND means it is
// already gone, which is the goal.
func (w *campaignWatch) Stop(ctx context.Context) error {
	w.cancel()
	<-w.done
	_, err := w.client.CampaignRemove(ctx, &agentpbv2.DataCampaignRemoveRequest{Name: w.name})
	switch status.Code(err) {
	case codes.OK, codes.NotFound:
		return nil
	case codes.Unimplemented:
		return errWatchAgentTooOld
	}
	return err
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd go && go test ./internal/cli/mcp/ -run 'CampaignWatch|DefaultWatchDetector' -race -v`
Expected: PASS, no race reports.

- [ ] **Step 7: Commit**

```bash
git add go/internal/cli/mcp/watch_backend.go go/internal/cli/mcp/watch_campaign.go go/internal/cli/mcp/watch_fake_test.go go/internal/cli/mcp/watch_campaign_test.go
git commit -m "feat(mcp): run watches as leased campaigns

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M9Sy7Q2qsNmQy7v8wtkdUD"
```

---

### Task 3: The shared notification-journal poller

**Files:**
- Modify: `go/internal/cli/mcp/watch_campaign.go`
- Modify: `go/internal/cli/mcp/watch_campaign_test.go`

**Interfaces:**
- Consumes: `campaignWatchBackend`, `campaignWatch.send`, `watchDataClient.publish`, `callLog`, `gapOnce`, `oldJournal` (Task 2).
- Produces: one poller per backend while any watch runs; each journal entry for a watch's campaign becomes `watchUpdate{Event: &watchEventUpdate{Kind: "entered", Classes: <entry detections>, OccurredAt: <entry time>}}`; a journal gap becomes `watchUpdate{Gap: "some detections may have been missed"}` on every watch.

- [ ] **Step 1: Write the failing tests**

Append to `go/internal/cli/mcp/watch_campaign_test.go` (add `"slices"` and `"strings"` to its imports):

```go
// nextEvent skips statuses until an event or gap arrives.
func nextEvent(t *testing.T, h watchHandle) watchUpdate {
	t.Helper()
	for {
		if u := nextUpdate(t, h); u.Event != nil || u.Gap != "" {
			return u
		}
	}
}

func TestCampaignPollerStartsBeforeDeploy(t *testing.T) {
	client := newWatchDataClient()
	b := fastCampaignBackend()
	h, err := b.Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop(context.Background())
	calls := client.callLog()
	if len(calls) < 2 || calls[0] != "events:" || calls[1] != "deploy:chat-0a1b2c3d-1" {
		t.Fatalf("the journal tail must be read before the deploy: %v", calls)
	}
	client.publish("chat-0a1b2c3d-1", data.NotificationDetection{Label: "person", Score: 0.91})
	u := nextEvent(t, h)
	if u.Event == nil || u.Event.Kind != "entered" || len(u.Event.Classes) != 1 || u.Event.Classes[0] != (watchClass{Label: "person", Score: 0.91}) || u.Event.OccurredAt.IsZero() {
		t.Fatalf("event %+v", u.Event)
	}
}

func TestCampaignPollerRoutesByCampaignAndReportsGaps(t *testing.T) {
	client := newWatchDataClient()
	b := fastCampaignBackend()
	conn := &grpcclient.AgentConnection{DataService: client}
	first, err := b.Start(context.Background(), conn, testWatchSpec("chat-0a1b2c3d-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Stop(context.Background())
	second, err := b.Start(context.Background(), conn, testWatchSpec("chat-0a1b2c3d-2"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Stop(context.Background())
	if n := strings.Count(strings.Join(client.callLog(), " "), "events: "); n != 1 {
		t.Fatalf("a second watch must reuse the poller, not take a new tail; empty-cursor reads: %d", n)
	}
	client.publish("people-all-cameras", data.NotificationDetection{Label: "person", Score: 0.99}) // not a watch
	client.publish("chat-0a1b2c3d-2", data.NotificationDetection{Label: "dog", Score: 0.8})
	client.publish("chat-0a1b2c3d-1", data.NotificationDetection{Label: "person", Score: 0.7})
	if u := nextEvent(t, first); u.Event == nil || u.Event.Classes[0].Label != "person" {
		t.Fatalf("first watch got %+v", u)
	}
	if u := nextEvent(t, second); u.Event == nil || u.Event.Classes[0].Label != "dog" {
		t.Fatalf("second watch got %+v", u)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !slices.Contains(client.callLog(), "events:epoch:3") {
		if time.Now().After(deadline) {
			t.Fatalf("the poller did not advance its cursor past the entries it read: %v", client.callLog())
		}
		time.Sleep(5 * time.Millisecond)
	}
	client.mu.Lock()
	client.gapOnce = true
	client.mu.Unlock()
	for _, h := range []watchHandle{first, second} {
		if u := nextEvent(t, h); u.Gap != "some detections may have been missed" {
			t.Fatalf("gap not reported: %+v", u)
		}
	}
}

func TestCampaignPollerRefusesAnAgentWithoutTheJournal(t *testing.T) {
	client := newWatchDataClient()
	client.oldJournal = true
	_, err := fastCampaignBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
	if !errors.Is(err, errWatchAgentTooOld) {
		t.Fatalf("got %v", err)
	}
	if slices.ContainsFunc(client.callLog(), func(c string) bool { return strings.HasPrefix(c, "deploy") }) {
		t.Fatal("a campaign was deployed to an agent without the notification journal")
	}
}

func TestCampaignPollerStopsWithTheLastWatch(t *testing.T) {
	client := newWatchDataClient()
	b := fastCampaignBackend()
	h, err := b.Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(client.callLog())
	time.Sleep(60 * time.Millisecond)
	if after := len(client.callLog()); after != before {
		t.Fatalf("the poller kept reading after the last watch stopped: %v", client.callLog()[before:])
	}
	client.deployErr = status.Error(codes.InvalidArgument, "no healthy camera matches")
	if _, err := b.Start(context.Background(), &grpcclient.AgentConnection{DataService: client}, testWatchSpec("chat-0a1b2c3d-2")); err == nil {
		t.Fatal("expected the deploy to fail")
	}
	before = len(client.callLog())
	time.Sleep(60 * time.Millisecond)
	if after := len(client.callLog()); after != before {
		t.Fatal("a failed first deploy left the poller running")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd go && go test ./internal/cli/mcp/ -run 'CampaignPoller' -v`
Expected: FAIL. No `events:` call happens before the deploy, and no event is ever routed.

- [ ] **Step 3: Implement the poller**

In `watch_campaign.go`:

(a) Add `"maps"` to the imports and a field to `campaignWatchBackend` (after `watches`):

```go
	poller  *campaignPoller // while any watch runs
```

(b) Add after `forget`:

```go
// campaignPoller reads one device's notification journal for every campaign
// watch and routes entries by campaign (design §6.3).
type campaignPoller struct {
	client agentpbv2.DataServiceClient
	cursor string
	cancel context.CancelFunc
	done   chan struct{}
}

// attach starts the poller before a watch's campaign is deployed. Its first
// read, with an empty cursor, returns the journal's tail, so a detection that
// fires right after the deploy is not missed. A poller for another client
// belongs to a connection the manager has already ended; it is replaced.
func (b *campaignWatchBackend) attach(ctx context.Context, client agentpbv2.DataServiceClient) error {
	b.mu.Lock()
	if b.poller != nil && b.poller.client == client {
		b.mu.Unlock()
		return nil
	}
	old := b.poller
	b.poller = nil
	b.mu.Unlock()
	if old != nil {
		old.cancel()
		<-old.done
	}
	callCtx, cancel := context.WithTimeout(ctx, watchCallTimeout)
	defer cancel()
	response, err := client.Events(callCtx, &agentpbv2.DataEventsRequest{NotificationsOnly: true})
	if err != nil {
		return watchDeviceError(err)
	}
	// An older agent ignores notifications_only and answers from its ordinary
	// event journal; it does not mark the response.
	if !response.GetNotifications() {
		return errWatchAgentTooOld
	}
	pollCtx, stop := context.WithCancel(context.Background())
	p := &campaignPoller{client: client, cursor: response.GetCursor(), cancel: stop, done: make(chan struct{})}
	b.mu.Lock()
	b.poller = p
	b.mu.Unlock()
	go b.poll(pollCtx, p)
	return nil
}

// detachIfIdle stops the poller once no watch is left.
func (b *campaignWatchBackend) detachIfIdle() {
	b.mu.Lock()
	p := b.poller
	if len(b.watches) > 0 || p == nil {
		b.mu.Unlock()
		return
	}
	b.poller = nil
	b.mu.Unlock()
	p.cancel()
	<-p.done
}

func (b *campaignWatchBackend) poll(ctx context.Context, p *campaignPoller) {
	defer close(p.done)
	ticker := time.NewTicker(b.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		callCtx, cancel := context.WithTimeout(ctx, watchCallTimeout)
		response, err := p.client.Events(callCtx, &agentpbv2.DataEventsRequest{Cursor: p.cursor, Replay: true, NotificationsOnly: true})
		cancel()
		if err != nil || !response.GetNotifications() {
			continue // transient; the next read resumes from the same cursor
		}
		var entries []data.CampaignNotification
		if json.Unmarshal(response.GetEventsJson(), &entries) != nil {
			continue
		}
		p.cursor = response.GetCursor()
		b.route(entries, response.GetGap())
	}
}

// route turns journal entries into events for the watches they belong to.
// Entries for other campaigns are ignored.
func (b *campaignWatchBackend) route(entries []data.CampaignNotification, gap bool) {
	b.mu.Lock()
	watches := maps.Clone(b.watches)
	b.mu.Unlock()
	if gap {
		for _, w := range watches {
			w.send(watchUpdate{Gap: "some detections may have been missed"})
		}
	}
	for _, entry := range entries {
		w := watches[entry.Campaign]
		if w == nil {
			continue
		}
		occurred, err := time.Parse(time.RFC3339Nano, entry.OccurredAt)
		if err != nil {
			occurred = time.Now()
		}
		classes := make([]watchClass, 0, len(entry.Detections))
		for _, detection := range entry.Detections {
			classes = append(classes, watchClass{Label: detection.Label, Score: detection.Score})
		}
		w.send(watchUpdate{Event: &watchEventUpdate{Kind: "entered", Classes: classes, OccurredAt: occurred}})
	}
}
```

(c) In `Start`, between `client := conn.DataService` and the `CampaignDeploy` call, insert:

```go
	if err := b.attach(ctx, client); err != nil {
		return nil, err
	}
```

and change the deploy-error branch to stop an idle poller:

```go
	if _, err := client.CampaignDeploy(ctx, &agentpbv2.DataCampaignDeployRequest{CampaignYaml: plan}); err != nil {
		b.detachIfIdle()
		return nil, watchDeviceError(err)
	}
```

(d) In `campaignWatch.run`, replace `defer w.backend.forget(w)` with:

```go
	defer w.backend.detachIfIdle()
	defer w.backend.forget(w)
```

(defers run last-in first-out: the watch is forgotten, then an idle poller stops.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd go && go test ./internal/cli/mcp/ -run 'CampaignWatch|CampaignPoller' -race -count=3 -v`
Expected: PASS three times, no race reports.

- [ ] **Step 5: Commit**

```bash
git add go/internal/cli/mcp/watch_campaign.go go/internal/cli/mcp/watch_campaign_test.go
git commit -m "feat(mcp): route device notifications to campaign watches

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M9Sy7Q2qsNmQy7v8wtkdUD"
```

---

### Task 4: The watch manager

**Files:**
- Create: `go/internal/cli/mcp/watch_manager.go`
- Create: `go/internal/cli/mcp/watch_manager_test.go`

**Interfaces:**
- Consumes: the Task 2 types and interfaces.
- Produces:
  - constants `maxActiveWatches = 2`, `watchEventBufferSize = 100`, `watchEventMethod = "notifications/wendy/watch_event"`, `watchStatusMethod = "notifications/wendy/watch_status"`, `watchStoppedReason = "stopped"`;
  - `var errWatchLimit, errWatchNotFound error`;
  - `type watchEvent struct { Sequence uint64; Kind string; Classes []watchClass; OccurredAt string }` (JSON `sequence`, `kind`, `classes`, `occurred_at`);
  - `type watchView struct { WatchID, Label, Camera, CameraName string; Classes []string; MinConfidence float64; State, Reason, StartedAt, LastEventAt string; LastSequence uint64 }` (JSON `watch_id`, `label`, `camera`, `camera_name`, `classes`, `min_confidence`, `state`, `reason,omitempty`, `started_at`, `last_event_at,omitempty`, `last_sequence`);
  - `func newWatchManager(backend watchBackend, notify func(method string, params map[string]any) error, revision func() uint64) *watchManager` with field `startWait time.Duration` (default 30 s);
  - methods `start(ctx, conn, revision uint64, spec watchSpec) (watchView, error)`, `stop(ctx, id, reason string) (watchView, error)`, `list() []watchView`, `freeSlots() int`, `events(ctx, id string, after uint64, wait time.Duration) (watchView, []watchEvent, bool, error)` (the bool is `gap`), `endStale(revision uint64)`, `shutdown()`.

- [ ] **Step 1: Write the failing tests**

Create `go/internal/cli/mcp/watch_manager_test.go`:

```go
package mcp

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

type fakeWatchHandle struct {
	updates   chan watchUpdate
	stopped   atomic.Int32
	stopWait  time.Duration
	closeOnce sync.Once
}

func (h *fakeWatchHandle) Updates() <-chan watchUpdate { return h.updates }
func (h *fakeWatchHandle) Stop(ctx context.Context) error {
	h.stopped.Add(1)
	if h.stopWait > 0 {
		select {
		case <-time.After(h.stopWait):
		case <-ctx.Done():
		}
	}
	h.closeOnce.Do(func() { close(h.updates) })
	return nil
}

type fakeWatchBackend struct {
	mu       sync.Mutex
	specs    []watchSpec
	handles  []*fakeWatchHandle
	err      error
	stopWait time.Duration
	started  chan struct{} // optional: Start blocks until it is closed
}

func (b *fakeWatchBackend) Start(ctx context.Context, _ *grpcclient.AgentConnection, spec watchSpec) (watchHandle, error) {
	if b.started != nil {
		<-b.started
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	h := &fakeWatchHandle{updates: make(chan watchUpdate, 256), stopWait: b.stopWait}
	b.specs = append(b.specs, spec)
	b.handles = append(b.handles, h)
	return h, nil
}

func (b *fakeWatchBackend) handle(i int) *fakeWatchHandle {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.handles[i]
}

type sentNotification struct {
	method string
	params map[string]any
}

type notificationLog struct {
	mu   sync.Mutex
	sent []sentNotification
	fail atomic.Int32 // fail this many sends
}

func (l *notificationLog) notify(method string, params map[string]any) error {
	if l.fail.Load() > 0 {
		l.fail.Add(-1)
		return errors.New("notification channel blocked")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sent = append(l.sent, sentNotification{method, params})
	return nil
}

func (l *notificationLog) waitFor(t *testing.T, match func(sentNotification) bool) sentNotification {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		for _, n := range l.sent {
			if match(n) {
				l.mu.Unlock()
				return n
			}
		}
		l.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected notification was not sent")
	return sentNotification{}
}

func newTestWatchManager(backend watchBackend) (*watchManager, *notificationLog, *atomic.Uint64) {
	log := &notificationLog{}
	revision := &atomic.Uint64{}
	revision.Store(1)
	m := newWatchManager(backend, log.notify, revision.Load)
	m.startWait = 100 * time.Millisecond
	return m, log, revision
}

func startTestWatch(t *testing.T, m *watchManager, label string) watchView {
	t.Helper()
	view, err := m.start(context.Background(), &grpcclient.AgentConnection{}, 1, watchSpec{CameraID: "v4l2:/dev/video0", CameraName: "Brio 101", Classes: []string{"person"}, MinConfidence: 0.5, Label: label})
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func waitState(t *testing.T, m *watchManager, id string, state watchState) watchView {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, v := range m.list() {
			if v.WatchID == id && v.State == string(state) {
				return v
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("watch %s never reached %s: %+v", id, state, m.list())
	return watchView{}
}

func TestWatchManagerAssignsIdentitiesAndCapsActiveWatches(t *testing.T) {
	backend := &fakeWatchBackend{}
	m, _, _ := newTestWatchManager(backend)
	first, second := startTestWatch(t, m, "front door"), startTestWatch(t, m, "garage")
	if first.WatchID != "w1" || second.WatchID != "w2" {
		t.Fatalf("ids %q %q", first.WatchID, second.WatchID)
	}
	if !regexp.MustCompile(`^chat-[0-9a-f]{8}-1$`).MatchString(backend.specs[0].Name) || !strings.HasSuffix(backend.specs[1].Name, "-2") {
		t.Fatalf("campaign names %q %q", backend.specs[0].Name, backend.specs[1].Name)
	}
	if first.State != string(watchPreparing) || m.freeSlots() != 0 {
		t.Fatalf("view %+v, free %d", first, m.freeSlots())
	}
	_, err := m.start(context.Background(), &grpcclient.AgentConnection{}, 1, watchSpec{Label: "third", Classes: []string{"person"}})
	if !errors.Is(err, errWatchLimit) || !strings.Contains(err.Error(), "front door") || !strings.Contains(err.Error(), "garage") {
		t.Fatalf("third watch: %v", err)
	}
	if _, err := m.stop(context.Background(), "w1", watchStoppedReason); err != nil {
		t.Fatal(err)
	}
	if backend.handle(0).stopped.Load() != 1 {
		t.Fatal("stop did not stop the backend watch")
	}
	if third := startTestWatch(t, m, "third"); third.WatchID != "w3" {
		t.Fatalf("a freed slot was not reused: %+v", third)
	}
	if _, err := m.stop(context.Background(), "w9", watchStoppedReason); !errors.Is(err, errWatchNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if v, err := m.stop(context.Background(), "w1", watchStoppedReason); err != nil || v.State != string(watchEnded) {
		t.Fatalf("stopping an ended watch must be harmless: %+v %v", v, err)
	}
}

func TestWatchManagerStartReturnsOnReadyOrAfterTheWait(t *testing.T) {
	backend := &fakeWatchBackend{}
	m, log, _ := newTestWatchManager(backend)
	m.startWait = time.Second
	go func() {
		for {
			backend.mu.Lock()
			n := len(backend.handles)
			backend.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		backend.handle(0).updates <- watchUpdate{Status: &watchStatusUpdate{State: watchReady}}
	}()
	if v := startTestWatch(t, m, "front door"); v.State != string(watchReady) {
		t.Fatalf("start returned %+v, want READY", v)
	}
	log.waitFor(t, func(n sentNotification) bool {
		return n.method == watchStatusMethod && n.params["state"] == "READY" && n.params["camera"] == "Brio 101"
	})
	m.startWait = 50 * time.Millisecond
	began := time.Now()
	if v := startTestWatch(t, m, "garage"); v.State != string(watchPreparing) || time.Since(began) < 50*time.Millisecond {
		t.Fatalf("start returned %+v after %s", v, time.Since(began))
	}
}

func TestWatchManagerBuffersLastHundredEvents(t *testing.T) {
	backend := &fakeWatchBackend{}
	m, log, _ := newTestWatchManager(backend)
	startTestWatch(t, m, "front door")
	h := backend.handle(0)
	for i := 0; i < 105; i++ {
		h.updates <- watchUpdate{Event: &watchEventUpdate{Kind: "entered", Classes: []watchClass{{Label: "person", Score: 0.9}}, OccurredAt: time.Now()}}
	}
	log.waitFor(t, func(n sentNotification) bool { return n.method == watchEventMethod && n.params["sequence"] == uint64(105) })
	_, events, gap, err := m.events(context.Background(), "w1", 0, 0)
	if err != nil || len(events) != 100 || events[0].Sequence != 6 || events[99].Sequence != 105 || !gap {
		t.Fatalf("got %d events from %d, gap %v, err %v", len(events), events[0].Sequence, gap, err)
	}
	_, events, gap, _ = m.events(context.Background(), "w1", 100, 0)
	if len(events) != 5 || gap {
		t.Fatalf("after 100: %d events, gap %v", len(events), gap)
	}
	if v := m.list()[0]; v.LastSequence != 105 || v.LastEventAt == "" {
		t.Fatalf("view %+v", v)
	}
}

func TestWatchManagerEventsWaitsForTheNextEvent(t *testing.T) {
	backend := &fakeWatchBackend{}
	m, _, _ := newTestWatchManager(backend)
	startTestWatch(t, m, "front door")
	go func() {
		time.Sleep(50 * time.Millisecond)
		backend.handle(0).updates <- watchUpdate{Event: &watchEventUpdate{Kind: "entered", OccurredAt: time.Now()}}
	}()
	began := time.Now()
	_, events, _, err := m.events(context.Background(), "w1", 0, 2*time.Second)
	if err != nil || len(events) != 1 || time.Since(began) > time.Second {
		t.Fatalf("events %+v err %v after %s", events, err, time.Since(began))
	}
	if _, events, _, _ := m.events(context.Background(), "w1", 1, 30*time.Millisecond); len(events) != 0 {
		t.Fatal("a wait with no new event must return empty")
	}
}

func TestWatchManagerCountsFailedNotifications(t *testing.T) {
	backend := &fakeWatchBackend{}
	m, log, _ := newTestWatchManager(backend)
	startTestWatch(t, m, "front door")
	log.fail.Store(1)
	backend.handle(0).updates <- watchUpdate{Event: &watchEventUpdate{Kind: "entered", OccurredAt: time.Now()}}
	backend.handle(0).updates <- watchUpdate{Status: &watchStatusUpdate{State: watchReady}}
	n := log.waitFor(t, func(n sentNotification) bool { return n.method == watchStatusMethod && n.params["state"] == "READY" })
	if n.params["missed_notifications"] != 1 {
		t.Fatalf("status %+v must report the missed event notification", n.params)
	}
	if _, events, _, _ := m.events(context.Background(), "w1", 0, 0); len(events) != 1 {
		t.Fatal("an event whose notification failed must still be in the buffer")
	}
}

func TestWatchManagerReportsGapsWithoutChangingState(t *testing.T) {
	backend := &fakeWatchBackend{}
	m, log, _ := newTestWatchManager(backend)
	startTestWatch(t, m, "front door")
	backend.handle(0).updates <- watchUpdate{Status: &watchStatusUpdate{State: watchReady}}
	backend.handle(0).updates <- watchUpdate{Gap: "some detections may have been missed"}
	log.waitFor(t, func(n sentNotification) bool {
		return n.method == watchStatusMethod && n.params["state"] == "READY" && n.params["reason"] == "some detections may have been missed"
	})
	if v := waitState(t, m, "w1", watchReady); v.Reason != "" {
		t.Fatalf("a gap must not become the watch's standing reason: %+v", v)
	}
}

func TestWatchManagerEndsWatchesWhenTheBackendEndsThem(t *testing.T) {
	backend := &fakeWatchBackend{}
	m, _, _ := newTestWatchManager(backend)
	startTestWatch(t, m, "front door")
	backend.handle(0).updates <- watchUpdate{Status: &watchStatusUpdate{State: watchEnded, Reason: "the device restarted or the watch expired"}}
	if v := waitState(t, m, "w1", watchEnded); v.Reason != "the device restarted or the watch expired" || m.freeSlots() != 2 {
		t.Fatalf("view %+v, free %d", v, m.freeSlots())
	}
}

func TestWatchManagerEndsStaleWatchesOnConnectionChange(t *testing.T) {
	backend := &fakeWatchBackend{}
	m, log, revision := newTestWatchManager(backend)
	startTestWatch(t, m, "front door")
	revision.Store(2)
	m.endStale(2)
	if v := waitState(t, m, "w1", watchEnded); v.Reason != "device changed" {
		t.Fatalf("view %+v", v)
	}
	log.waitFor(t, func(n sentNotification) bool { return n.params["state"] == "ENDED" && n.params["reason"] == "device changed" })
	deadline := time.Now().Add(2 * time.Second)
	for backend.handle(0).stopped.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if backend.handle(0).stopped.Load() == 0 {
		t.Fatal("the stale watch's renewals were not stopped")
	}
}

func TestWatchManagerEndsWatchStartedDuringConnectionChange(t *testing.T) {
	backend := &fakeWatchBackend{started: make(chan struct{})}
	m, _, revision := newTestWatchManager(backend)
	done := make(chan watchView, 1)
	go func() {
		v, _ := m.start(context.Background(), &grpcclient.AgentConnection{}, 1, watchSpec{Classes: []string{"person"}, Label: "front door"})
		done <- v
	}()
	time.Sleep(20 * time.Millisecond)
	revision.Store(2)
	m.endStale(2)
	close(backend.started)
	v := <-done
	if v.State != string(watchEnded) || v.Reason != "device changed" {
		t.Fatalf("a watch deployed across a connection change must end: %+v", v)
	}
	if backend.handle(0).stopped.Load() == 0 {
		t.Fatal("its backend watch was not stopped")
	}
}

func TestWatchManagerShutdownStopsConcurrently(t *testing.T) {
	backend := &fakeWatchBackend{stopWait: 400 * time.Millisecond}
	m, _, _ := newTestWatchManager(backend)
	startTestWatch(t, m, "front door")
	startTestWatch(t, m, "garage")
	began := time.Now()
	m.shutdown()
	if elapsed := time.Since(began); elapsed > 700*time.Millisecond {
		t.Fatalf("shutdown took %s; removals must run concurrently", elapsed)
	}
	if backend.handle(0).stopped.Load() != 1 || backend.handle(1).stopped.Load() != 1 {
		t.Fatal("not every watch was stopped")
	}
	if _, err := m.start(context.Background(), &grpcclient.AgentConnection{}, 1, watchSpec{Classes: []string{"person"}}); err == nil {
		t.Fatal("a closed manager must refuse new watches")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd go && go test ./internal/cli/mcp/ -run 'WatchManager' -v`
Expected: build failure, `undefined: newWatchManager`.

- [ ] **Step 3: Implement**

Create `go/internal/cli/mcp/watch_manager.go`:

```go
package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

const (
	maxActiveWatches     = 2
	watchEventBufferSize = 100
	watchEndedRetained   = 10
	watchStartWait       = 30 * time.Second
	// The MCP client gives the server 2 s after closing stdin before SIGTERM.
	watchShutdownTimeout = 1500 * time.Millisecond
	watchEventMethod     = "notifications/wendy/watch_event"
	watchStatusMethod    = "notifications/wendy/watch_status"
	watchStoppedReason   = "stopped"
)

var (
	errWatchLimit    = errors.New("two watches are already active; stop one first")
	errWatchNotFound = errors.New("no watch with that id in this session")
)

// watchEvent is one buffered event, numbered per watch.
type watchEvent struct {
	Sequence   uint64       `json:"sequence"`
	Kind       string       `json:"kind"`
	Classes    []watchClass `json:"classes"`
	OccurredAt string       `json:"occurred_at"`
}

// watchView is a watch as tools and clients see it.
type watchView struct {
	WatchID       string   `json:"watch_id"`
	Label         string   `json:"label"`
	Camera        string   `json:"camera"`
	CameraName    string   `json:"camera_name"`
	Classes       []string `json:"classes"`
	MinConfidence float64  `json:"min_confidence"`
	State         string   `json:"state"`
	Reason        string   `json:"reason,omitempty"`
	StartedAt     string   `json:"started_at"`
	LastEventAt   string   `json:"last_event_at,omitempty"`
	LastSequence  uint64   `json:"last_sequence"`
}

// watchRecord is one watch. Its fields are guarded by the manager's mu.
type watchRecord struct {
	id          string
	spec        watchSpec
	revision    uint64 // the connection the watch was started on
	state       watchState
	reason      string
	startedAt   time.Time
	lastEventAt time.Time
	events      []watchEvent
	sequence    uint64
	missed      int // notifications that failed, reported with the next status
	handle      watchHandle
	changed     chan struct{} // closed and replaced on every change
}

// watchManager owns one MCP server's watches (design §6.1). It never holds mu
// while calling the backend or sending a notification, and it is never called
// with the server's mu held, so it cannot deadlock against connection changes.
type watchManager struct {
	backend   watchBackend
	notify    func(method string, params map[string]any) error
	revision  func() uint64
	serverID  string
	startWait time.Duration

	mu      sync.Mutex
	next    int
	watches map[string]*watchRecord
	order   []string
	closed  bool
}

func newWatchManager(backend watchBackend, notify func(method string, params map[string]any) error, revision func() uint64) *watchManager {
	var id [4]byte
	_, _ = rand.Read(id[:])
	return &watchManager{backend: backend, notify: notify, revision: revision, serverID: hex.EncodeToString(id[:]), startWait: watchStartWait, watches: map[string]*watchRecord{}}
}

func (m *watchManager) activeLocked() []*watchRecord {
	var active []*watchRecord
	for _, id := range m.order {
		if rec := m.watches[id]; rec.state != watchEnded {
			active = append(active, rec)
		}
	}
	return active
}

func (m *watchManager) freeSlots() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maxActiveWatches - len(m.activeLocked())
}

// start deploys a watch and waits up to startWait for it to leave PREPARING. A
// first watch on a fresh device installs the detector, which takes minutes;
// its READY then arrives as a notification.
func (m *watchManager) start(ctx context.Context, conn *grpcclient.AgentConnection, revision uint64, spec watchSpec) (watchView, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return watchView{}, errors.New("the watch manager has shut down")
	}
	if active := m.activeLocked(); len(active) >= maxActiveWatches {
		labels := make([]string, 0, len(active))
		for _, rec := range active {
			labels = append(labels, fmt.Sprintf("%s (%s)", rec.id, rec.spec.Label))
		}
		m.mu.Unlock()
		return watchView{}, fmt.Errorf("%w: %s", errWatchLimit, strings.Join(labels, ", "))
	}
	m.next++
	spec.Name = fmt.Sprintf("chat-%s-%d", m.serverID, m.next)
	// The record holds the slot while the campaign deploys.
	rec := &watchRecord{id: fmt.Sprintf("w%d", m.next), spec: spec, revision: revision, state: watchPreparing, startedAt: time.Now(), changed: make(chan struct{})}
	m.watches[rec.id] = rec
	m.order = append(m.order, rec.id)
	m.mu.Unlock()

	handle, err := m.backend.Start(ctx, conn, spec)
	m.mu.Lock()
	if err != nil {
		m.forgetLocked(rec.id)
		m.mu.Unlock()
		return watchView{}, err
	}
	rec.handle = handle
	if rec.state != watchEnded && m.revision() != rec.revision {
		// The device changed while this watch deployed: endStale ran before
		// the record had a handle to stop.
		m.endLocked(rec, "device changed")
	}
	ended := rec.state == watchEnded
	m.mu.Unlock()
	if ended {
		m.stopHandle(handle, time.Second)
		m.sendStatus(rec, "")
		return m.view(rec), nil
	}
	m.sendStatus(rec, "")
	go m.follow(rec, handle)
	return m.waitWhilePreparing(ctx, rec), nil
}

func (m *watchManager) forgetLocked(id string) {
	delete(m.watches, id)
	for i, existing := range m.order {
		if existing == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

func (m *watchManager) follow(rec *watchRecord, handle watchHandle) {
	for update := range handle.Updates() {
		m.apply(rec, update)
	}
}

func (m *watchManager) apply(rec *watchRecord, u watchUpdate) {
	m.mu.Lock()
	if rec.state == watchEnded {
		m.mu.Unlock()
		return
	}
	switch {
	case u.Event != nil:
		rec.sequence++
		event := watchEvent{Sequence: rec.sequence, Kind: u.Event.Kind, Classes: u.Event.Classes, OccurredAt: u.Event.OccurredAt.UTC().Format(time.RFC3339Nano)}
		if event.Classes == nil {
			event.Classes = []watchClass{}
		}
		rec.events = append(rec.events, event)
		if len(rec.events) > watchEventBufferSize {
			rec.events = rec.events[len(rec.events)-watchEventBufferSize:]
		}
		rec.lastEventAt = u.Event.OccurredAt
		m.signalLocked(rec)
		m.mu.Unlock()
		m.deliver(rec, watchEventMethod, map[string]any{"watch_id": rec.id, "label": rec.spec.Label, "sequence": event.Sequence, "kind": event.Kind, "classes": event.Classes, "occurred_at": event.OccurredAt})
	case u.Status != nil && u.Status.State == watchEnded:
		handle := rec.handle
		m.endLocked(rec, u.Status.Reason)
		m.mu.Unlock()
		m.stopHandle(handle, 5*time.Second)
		m.sendStatus(rec, "")
	case u.Status != nil:
		changed := rec.state != u.Status.State || rec.reason != u.Status.Reason
		rec.state, rec.reason = u.Status.State, u.Status.Reason
		m.signalLocked(rec)
		m.mu.Unlock()
		if changed {
			m.sendStatus(rec, "")
		}
	case u.Gap != "":
		m.mu.Unlock()
		m.sendStatus(rec, u.Gap)
	default:
		m.mu.Unlock()
	}
}

func (m *watchManager) signalLocked(rec *watchRecord) {
	close(rec.changed)
	rec.changed = make(chan struct{})
}

func (m *watchManager) endLocked(rec *watchRecord, reason string) {
	rec.state, rec.reason = watchEnded, reason
	m.signalLocked(rec)
	// Keep a few ended watches, so a client can still learn how they ended.
	var ended []string
	for _, id := range m.order {
		if m.watches[id].state == watchEnded {
			ended = append(ended, id)
		}
	}
	for len(ended) > watchEndedRetained {
		m.forgetLocked(ended[0])
		ended = ended[1:]
	}
}

func (m *watchManager) stopHandle(handle watchHandle, timeout time.Duration) {
	if handle == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = handle.Stop(ctx)
}

// stop ends a watch and removes it from the device. The record is ended first,
// so no event arrives after watch_stop returns.
func (m *watchManager) stop(ctx context.Context, id, reason string) (watchView, error) {
	m.mu.Lock()
	rec := m.watches[id]
	if rec == nil {
		m.mu.Unlock()
		return watchView{}, errWatchNotFound
	}
	if rec.state == watchEnded {
		view := m.viewLocked(rec)
		m.mu.Unlock()
		return view, nil
	}
	handle := rec.handle
	m.endLocked(rec, reason)
	m.mu.Unlock()
	var err error
	if handle != nil {
		err = handle.Stop(ctx)
	}
	m.sendStatus(rec, "")
	return m.view(rec), err
}

// endStale ends every watch started on another connection. That connection is
// already closed, so nothing reaches the old device: its campaigns lapse within
// lease + 5 s (design §6.1).
func (m *watchManager) endStale(revision uint64) {
	m.mu.Lock()
	var ended []*watchRecord
	for _, id := range m.order {
		rec := m.watches[id]
		if rec.state != watchEnded && rec.revision != revision && rec.handle != nil {
			m.endLocked(rec, "device changed")
			ended = append(ended, rec)
		}
	}
	m.mu.Unlock()
	for _, rec := range ended {
		go m.stopHandle(rec.handle, time.Second)
		m.sendStatus(rec, "")
	}
}

// shutdown removes every active watch when the server exits. Removals run
// concurrently under one short deadline; a lease covers anything the device
// did not receive.
func (m *watchManager) shutdown() {
	m.mu.Lock()
	m.closed = true
	var handles []watchHandle
	for _, rec := range m.activeLocked() {
		if rec.handle != nil {
			handles = append(handles, rec.handle)
		}
		m.endLocked(rec, "the session ended")
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, handle := range handles {
		wg.Add(1)
		go func(h watchHandle) {
			defer wg.Done()
			m.stopHandle(h, watchShutdownTimeout)
		}(handle)
	}
	wg.Wait()
}

func (m *watchManager) waitWhilePreparing(ctx context.Context, rec *watchRecord) watchView {
	timer := time.NewTimer(m.startWait)
	defer timer.Stop()
	for {
		m.mu.Lock()
		if rec.state != watchPreparing {
			view := m.viewLocked(rec)
			m.mu.Unlock()
			return view
		}
		changed := rec.changed
		m.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			return m.view(rec)
		case <-ctx.Done():
			return m.view(rec)
		}
	}
}

// events returns buffered events after a sequence number, waiting up to wait
// for one to arrive. gap reports events that left the buffer unread.
func (m *watchManager) events(ctx context.Context, id string, after uint64, wait time.Duration) (watchView, []watchEvent, bool, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		m.mu.Lock()
		rec := m.watches[id]
		if rec == nil {
			m.mu.Unlock()
			return watchView{}, nil, false, errWatchNotFound
		}
		events := []watchEvent{}
		for _, event := range rec.events {
			if event.Sequence > after {
				events = append(events, event)
			}
		}
		gap := len(rec.events) > 0 && rec.events[0].Sequence > after+1
		changed := rec.changed
		if len(events) > 0 || rec.state == watchEnded || wait <= 0 {
			view := m.viewLocked(rec)
			m.mu.Unlock()
			return view, events, gap, nil
		}
		m.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			wait = 0
		case <-ctx.Done():
			wait = 0
		}
	}
}

func (m *watchManager) list() []watchView {
	m.mu.Lock()
	defer m.mu.Unlock()
	views := make([]watchView, 0, len(m.order))
	for _, id := range m.order {
		views = append(views, m.viewLocked(m.watches[id]))
	}
	return views
}

func (m *watchManager) view(rec *watchRecord) watchView {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.viewLocked(rec)
}

func (m *watchManager) viewLocked(rec *watchRecord) watchView {
	view := watchView{
		WatchID: rec.id, Label: rec.spec.Label, Camera: rec.spec.CameraID, CameraName: rec.spec.CameraName,
		Classes: rec.spec.Classes, MinConfidence: rec.spec.MinConfidence, State: string(rec.state), Reason: rec.reason,
		StartedAt: rec.startedAt.UTC().Format(time.RFC3339), LastSequence: rec.sequence,
	}
	if !rec.lastEventAt.IsZero() {
		view.LastEventAt = rec.lastEventAt.UTC().Format(time.RFC3339Nano)
	}
	return view
}

// sendStatus sends the watch's status. reason, when set, replaces the standing
// reason for this one notification (a gap does not change the watch's state).
func (m *watchManager) sendStatus(rec *watchRecord, reason string) {
	m.mu.Lock()
	if reason == "" {
		reason = rec.reason
	}
	params := map[string]any{"watch_id": rec.id, "label": rec.spec.Label, "state": string(rec.state), "reason": reason, "camera": rec.spec.CameraName, "watching": rec.spec.Classes}
	if rec.missed > 0 {
		params["missed_notifications"] = rec.missed
		rec.missed = 0
	}
	m.mu.Unlock()
	m.deliver(rec, watchStatusMethod, params)
}

// deliver sends one notification. mcp-go's stdio session queues 100 and fails
// a send when full; the failure is reported with the watch's next status.
func (m *watchManager) deliver(rec *watchRecord, method string, params map[string]any) {
	if m.notify == nil {
		return
	}
	if err := m.notify(method, params); err != nil {
		m.mu.Lock()
		rec.missed++
		m.mu.Unlock()
	}
}
```

Note: `TestWatchManagerCountsFailedNotifications` checks `missed_notifications == 1` as an `int`, which is what `rec.missed` is.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd go && go test ./internal/cli/mcp/ -run 'WatchManager' -race -count=3 -v`
Expected: PASS three times, no race reports.

- [ ] **Step 5: Commit**

```bash
git add go/internal/cli/mcp/watch_manager.go go/internal/cli/mcp/watch_manager_test.go
git commit -m "feat(mcp): manage a session's watches and their notifications

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M9Sy7Q2qsNmQy7v8wtkdUD"
```

---

### Task 5: The watch tools, server wiring and docs

**Files:**
- Create: `go/internal/cli/mcp/watch_tools.go`
- Create: `go/internal/cli/mcp/watch_tools_test.go`
- Modify: `go/internal/cli/mcp/server.go` (`mcpServer` struct, `setConnectionLocked`, `newProtocolServer`, `Start`)
- Modify: `go/internal/cli/mcp/tool_groups.go` (`hardware` group)
- Modify: `go/internal/cli/mcp/tools_guide.go` (tool-group lines and a new section)
- Modify: `go/internal/cli/assets/docs/integrations/mcp.mdx`

**Interfaces:**
- Consumes: everything from Tasks 1–4; `watchDataClient` (Task 2).
- Produces: tools `watch_sources`, `watch_start`, `watch_list`, `watch_stop`, `watch_events`; `mcpServer.watches *watchManager` (guarded by `s.mu`); `func (s *mcpServer) startWatches(srv *server.MCPServer) func()`; `func (s *mcpServer) connWithRevision() (*grpcclient.AgentConnection, uint64)`; helper `func watchCameraName(*agentpbv2.DataSource) string`.

- [ ] **Step 1: Write the failing tests**

Create `go/internal/cli/mcp/watch_tools_test.go`:

```go
package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var watchToolNames = []string{"watch_sources", "watch_start", "watch_list", "watch_stop", "watch_events"}

// watchToolServer is a server connected to a fake device, with a fast backend
// and a notification log in place of the stdio client.
func watchToolServer(t *testing.T) (*mcpServer, *watchDataClient, *notificationLog) {
	t.Helper()
	client := newWatchDataClient()
	s := New(&config.Config{}, nil)
	s.SetConn(&grpcclient.AgentConnection{DataService: client})
	log := &notificationLog{}
	m := newWatchManager(fastCampaignBackend(), log.notify, func() uint64 { _, r := s.connWithRevision(); return r })
	m.startWait = 2 * time.Second
	s.mu.Lock()
	s.watches = m
	s.mu.Unlock()
	t.Cleanup(m.shutdown)
	return s, client, log
}

func callWatchTool(t *testing.T, s *mcpServer, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	handlers := map[string]server.ToolHandlerFunc{
		"watch_sources": s.handleWatchSources, "watch_start": s.handleWatchStart, "watch_list": s.handleWatchList,
		"watch_stop": s.handleWatchStop, "watch_events": s.handleWatchEvents,
	}
	result, err := handlers[name](context.Background(), callToolReq(name, args))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result.StructuredContent)
	var out map[string]any
	_ = json.Unmarshal(encoded, &out)
	return out, result.IsError
}

func TestWatchToolsAreHardwareToolsWithTheRightAnnotations(t *testing.T) {
	s := New(&config.Config{}, nil)
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range watchToolNames {
		tool := srv.GetTool(name)
		if tool == nil || !slices.Contains(toolGroups["hardware"], name) {
			t.Fatalf("%s is not a registered hardware tool", name)
		}
		readOnly := tool.Tool.Annotations.ReadOnlyHint != nil && *tool.Tool.Annotations.ReadOnlyHint
		if mutating := name == "watch_start" || name == "watch_stop"; readOnly == mutating {
			t.Fatalf("%s read-only = %v", name, readOnly)
		}
	}
}

func TestWatchStartValidatesBeforeDeploying(t *testing.T) {
	s, client, _ := watchToolServer(t)
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no camera", map[string]any{"classes": []any{"person"}}, "camera"},
		{"unknown camera", map[string]any{"camera": "v4l2:/dev/video9", "classes": []any{"person"}}, "v4l2:/dev/video0 (Brio 101)"},
		{"unhealthy camera", map[string]any{"camera": "v4l2:/dev/video2", "classes": []any{"person"}}, "v4l2:/dev/video0 (Brio 101)"},
		{"no classes", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{}}, "1 to 10"},
		{"eleven classes", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person", "car", "dog", "cat", "bus", "boat", "bird", "cow", "horse", "sheep", "bear"}}, "1 to 10"},
		{"unknown class", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"motorcycle"}}, "watch_sources"},
		{"duplicate class", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person", "person"}}, "more than once"},
		{"confidence too low", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}, "min_confidence": 0.2}, "0.3"},
		{"confidence too high", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}, "min_confidence": 0.96}, "0.95"},
		{"long label", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}, "label": strings.Repeat("x", 65)}, "64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, isErr := callWatchTool(t, s, "watch_start", tc.args)
			if !isErr || out["error_code"] != "INVALID_ARGUMENT" || !strings.Contains(out["message"].(string), tc.want) {
				t.Fatalf("got %v %v, want INVALID_ARGUMENT mentioning %q", isErr, out, tc.want)
			}
		})
	}
	if len(client.callLog()) != 0 {
		t.Fatalf("an invalid request reached the device: %v", client.callLog())
	}
}

func TestWatchToolsValidateIDsAndWaits(t *testing.T) {
	s, _, _ := watchToolServer(t)
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"watch_stop", map[string]any{}},
		{"watch_events", map[string]any{}},
		{"watch_events", map[string]any{"watch_id": "w1", "wait_seconds": 121}},
		{"watch_events", map[string]any{"watch_id": "w1", "after_sequence": -1}},
	} {
		if out, isErr := callWatchTool(t, s, tc.tool, tc.args); !isErr || out["error_code"] != "INVALID_ARGUMENT" {
			t.Fatalf("%s %v: %v", tc.tool, tc.args, out)
		}
	}
	if out, isErr := callWatchTool(t, s, "watch_events", map[string]any{"watch_id": "w5"}); !isErr || out["error_code"] != "NOT_FOUND" {
		t.Fatalf("unknown watch: %v", out)
	}
	if out, isErr := callWatchTool(t, s, "watch_list", map[string]any{}); isErr || out["free_slots"] != float64(2) {
		t.Fatalf("list: %v", out)
	}
}

func TestWatchToolsNeedAConnection(t *testing.T) {
	s := New(&config.Config{}, nil)
	m := newWatchManager(fastCampaignBackend(), nil, func() uint64 { return 0 })
	s.mu.Lock()
	s.watches = m
	s.mu.Unlock()
	for _, name := range []string{"watch_sources", "watch_start"} {
		if out, isErr := callWatchTool(t, s, name, map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}}); !isErr || out["error_code"] != "NOT_CONNECTED" {
			t.Fatalf("%s: %v", name, out)
		}
	}
}

func TestWatchToolsEndToEnd(t *testing.T) {
	s, client, log := watchToolServer(t)
	sources, _ := callWatchTool(t, s, "watch_sources", nil)
	cameras := sources["cameras"].([]any)
	if len(cameras) != 1 || cameras[0].(map[string]any)["name"] != "Brio 101" || sources["free_slots"] != float64(2) {
		t.Fatalf("sources %v", sources)
	}
	if detector := sources["detector"].(map[string]any); detector["id"] != "dfine-nano-coco" || len(detector["labels"].([]any)) != 80 {
		t.Fatalf("detector %v", detector)
	}
	started, isErr := callWatchTool(t, s, "watch_start", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}, "min_confidence": 0.6})
	if isErr || started["watch_id"] != "w1" || started["label"] != "Brio 101" || started["state"] != "READY" || started["next_step"] == "" {
		t.Fatalf("start %v", started)
	}
	names := client.deployedNames()
	if len(names) != 1 {
		t.Fatalf("deployed %v", names)
	}
	name := names[0]
	client.publish(name, data.NotificationDetection{Label: "person", Score: 0.91})
	events, _ := callWatchTool(t, s, "watch_events", map[string]any{"watch_id": "w1", "wait_seconds": 2})
	list := events["events"].([]any)
	if len(list) != 1 || events["state"] != "READY" || events["next_sequence"] != float64(1) {
		t.Fatalf("events %v", events)
	}
	log.waitFor(t, func(n sentNotification) bool { return n.method == watchEventMethod && n.params["watch_id"] == "w1" })
	listed, _ := callWatchTool(t, s, "watch_list", nil)
	if watches := listed["watches"].([]any); len(watches) != 1 || listed["free_slots"] != float64(1) {
		t.Fatalf("list %v", listed)
	}
	stopped, isErr := callWatchTool(t, s, "watch_stop", map[string]any{"watch_id": "w1"})
	if isErr || stopped["state"] != "ENDED" || stopped["removed"] != true {
		t.Fatalf("stop %v", stopped)
	}
	if _, ok := client.campaign(name); ok {
		t.Fatal("watch_stop left the campaign on the device")
	}
	if out, isErr := callWatchTool(t, s, "watch_stop", map[string]any{"watch_id": "w7"}); !isErr || out["error_code"] != "NOT_FOUND" {
		t.Fatalf("unknown watch: %v", out)
	}
}

func TestWatchStartOnAnOldAgent(t *testing.T) {
	s, client, _ := watchToolServer(t)
	client.deployErr = status.Error(codes.InvalidArgument, "parsing campaign YAML: yaml: unmarshal errors:\n  line 3: field lease not found in type data.Campaign")
	out, isErr := callWatchTool(t, s, "watch_start", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}})
	if !isErr || out["error_code"] != "UNSUPPORTED" || !strings.Contains(out["message"].(string), "wendy device update") {
		t.Fatalf("got %v", out)
	}
	if listed, _ := callWatchTool(t, s, "watch_list", nil); len(listed["watches"].([]any)) != 0 || listed["free_slots"] != float64(2) {
		t.Fatalf("a refused watch must leave nothing: %v", listed)
	}
}

func TestWatchesEndWhenTheDeviceChanges(t *testing.T) {
	s, _, _ := watchToolServer(t)
	if _, isErr := callWatchTool(t, s, "watch_start", map[string]any{"camera": "v4l2:/dev/video0", "classes": []any{"person"}}); isErr {
		t.Fatal("start failed")
	}
	s.SetConn(&grpcclient.AgentConnection{DataService: newWatchDataClient()})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		listed, _ := callWatchTool(t, s, "watch_list", nil)
		if w := listed["watches"].([]any)[0].(map[string]any); w["state"] == "ENDED" && w["reason"] == "device changed" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("watch did not end after the connection changed")
}

func TestWatchCameraNameDropsTheTransport(t *testing.T) {
	for detail, want := range map[string]string{"Brio 101 VIDEO_TRANSPORT_USB": "Brio 101", "Front door": "Front door", "": "v4l2:/dev/video0"} {
		if got := watchCameraName("v4l2:/dev/video0", detail); got != want {
			t.Fatalf("%q -> %q, want %q", detail, got, want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd go && go test ./internal/cli/mcp/ -run 'WatchTools|WatchStart|WatchesEnd|WatchCameraName' -v`
Expected: build failure, `s.handleWatchSources undefined` (and `s.watches`, `connWithRevision`).

- [ ] **Step 3: Implement the tools**

Create `go/internal/cli/mcp/watch_tools.go`:

```go
package mcp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// Camera watches (design §6.5): a detector runs on the device and this session
// hears when one of the watched classes appears. Nothing is recorded or
// uploaded, and watches end with the session.
func (s *mcpServer) registerWatchTools(srv *server.MCPServer) {
	add := func(name string, handler server.ToolHandlerFunc, opts ...mcpgo.ToolOption) {
		opts = append(opts, localOnly()...)
		srv.AddTool(mcpgo.NewTool(name, opts...), handler)
	}
	add("watch_sources", s.handleWatchSources, append([]mcpgo.ToolOption{
		mcpgo.WithDescription("List what a camera watch can use: healthy cameras on the connected device (id, name), the detector watches run (id, model, class labels), and free watch slots. Call before watch_start."),
	}, readOnly()...)...)
	add("watch_start", s.handleWatchStart, append([]mcpgo.ToolOption{
		mcpgo.WithDescription("Watch a camera on the connected device for some classes of object, for example a person at the door. A detector runs on the device and this session is notified when a watched class appears; nothing is recorded or uploaded. At most two watches run per session, and they end when the session ends. Returns when the watch is READY or ERROR, or after 30 s while it is still PREPARING: a first watch on a device installs the detector, which takes minutes."),
		mcpgo.WithString("camera", mcpgo.Description("Camera id from watch_sources"), mcpgo.Required()),
		mcpgo.WithArray("classes", mcpgo.Description(`1-10 class labels from the detector's labels in watch_sources, for example ["person"]`), mcpgo.Required(), mcpgo.WithStringItems(), mcpgo.MinItems(1), mcpgo.MaxItems(10)),
		mcpgo.WithNumber("min_confidence", mcpgo.Description("Minimum detection score, 0.3 to 0.95; default 0.5"), mcpgo.Min(0.3), mcpgo.Max(0.95)),
		mcpgo.WithString("label", mcpgo.Description(`Short name shown to the user, for example "front door"; default the camera's name`), mcpgo.MaxLength(64)),
	}, mutating()...)...)
	add("watch_list", s.handleWatchList, append([]mcpgo.ToolOption{
		mcpgo.WithDescription("List this session's camera watches with state, classes and last event time, including recently ended ones and why they ended."),
	}, readOnly()...)...)
	add("watch_stop", s.handleWatchStop, append([]mcpgo.ToolOption{
		mcpgo.WithDescription("Stop a camera watch and remove its detector from the device."),
		mcpgo.WithString("watch_id", mcpgo.Description("Watch id from watch_start or watch_list"), mcpgo.Required()),
	}, append(mutating(), idempotent()...)...)...)
	add("watch_events", s.handleWatchEvents, append([]mcpgo.ToolOption{
		mcpgo.WithDescription("Read a watch's events after a sequence number from its buffer of the last 100, with its current state. wait_seconds waits for the next event. For clients that do not show this server's watch notifications."),
		mcpgo.WithString("watch_id", mcpgo.Description("Watch id from watch_start or watch_list"), mcpgo.Required()),
		mcpgo.WithNumber("after_sequence", mcpgo.Description("Return events after this sequence number; default 0 (all buffered)"), mcpgo.Min(0)),
		mcpgo.WithNumber("wait_seconds", mcpgo.Description("Wait up to this long for a new event, 0 to 120; default 0"), mcpgo.Min(0), mcpgo.Max(120)),
	}, readOnly()...)...)
}

func (s *mcpServer) watchManager() *watchManager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.watches
}

func (s *mcpServer) connWithRevision() (*grpcclient.AgentConnection, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conn, s.connRevision
}

type watchCamera struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func watchCameras(ctx context.Context, conn *grpcclient.AgentConnection) ([]watchCamera, error) {
	if conn.DataService == nil {
		return nil, errWatchAgentTooOld
	}
	response, err := conn.DataService.Sources(ctx, &agentpbv2.DataSourcesRequest{})
	if err != nil {
		return nil, err
	}
	cameras := []watchCamera{}
	for _, source := range response.GetSources() {
		if source.GetKind() == "camera" && source.GetHealthy() {
			cameras = append(cameras, watchCamera{ID: source.GetId(), Name: watchCameraName(source.GetId(), source.GetDetail())})
		}
	}
	return cameras, nil
}

// watchCameraName is the camera's own name. The agent's source detail appends
// the transport ("Brio 101 VIDEO_TRANSPORT_USB"), which is not part of it.
func watchCameraName(id, detail string) string {
	name := strings.TrimSpace(detail)
	if i := strings.LastIndex(name, " VIDEO_TRANSPORT_"); i >= 0 {
		name = strings.TrimSpace(name[:i])
	}
	if name == "" {
		return id
	}
	return name
}

func watchErrorResult(err error) *mcpgo.CallToolResult {
	switch {
	case errors.Is(err, errWatchAgentTooOld):
		return errResult(errCodeUnsupported, errWatchAgentTooOld.Error())
	case errors.Is(err, errWatchNotFound):
		return errResult(errCodeNotFound, err.Error())
	case errors.Is(err, errWatchLimit):
		return errResult(errCodeInvalidArgument, err.Error())
	}
	return errResult(codeFromGRPC(err), grpcErrString(err))
}

func (s *mcpServer) handleWatchSources(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	m := s.watchManager()
	if m == nil {
		return errResult(errCodeUnsupported, "watches are unavailable in this server"), nil
	}
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	cameras, err := watchCameras(ctx, conn)
	if err != nil {
		return watchErrorResult(err), nil
	}
	d := defaultWatchDetector
	return okResult(map[string]any{"cameras": cameras, "detector": map[string]any{"id": d.ID, "model": d.Model, "labels": d.Labels}, "free_slots": m.freeSlots()}), nil
}

func (s *mcpServer) handleWatchStart(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	m := s.watchManager()
	if m == nil {
		return errResult(errCodeUnsupported, "watches are unavailable in this server"), nil
	}
	conn, revision := s.connWithRevision()
	if conn == nil {
		return errNotConnected(), nil
	}
	camera := strings.TrimSpace(req.GetString("camera", ""))
	if camera == "" {
		return errResult(errCodeInvalidArgument, "camera is required; call watch_sources for camera ids"), nil
	}
	classes := req.GetStringSlice("classes", nil)
	if len(classes) < 1 || len(classes) > 10 {
		return errResult(errCodeInvalidArgument, "classes must list 1 to 10 of the detector's labels"), nil
	}
	seen := map[string]bool{}
	for _, class := range classes {
		if !defaultWatchDetector.hasLabel(class) {
			return errResultf(errCodeInvalidArgument, "%q is not one of the detector's labels; call watch_sources for them", class), nil
		}
		if seen[class] {
			return errResultf(errCodeInvalidArgument, "%q is listed more than once", class), nil
		}
		seen[class] = true
	}
	confidence := req.GetFloat("min_confidence", 0.5)
	if math.IsNaN(confidence) || confidence < 0.3 || confidence > 0.95 {
		return errResult(errCodeInvalidArgument, "min_confidence must be from 0.3 to 0.95"), nil
	}
	label := strings.TrimSpace(req.GetString("label", ""))
	if len(label) > 64 {
		return errResult(errCodeInvalidArgument, "label must be at most 64 bytes"), nil
	}
	cameras, err := watchCameras(ctx, conn)
	if err != nil {
		return watchErrorResult(err), nil
	}
	var chosen *watchCamera
	known := make([]string, 0, len(cameras))
	for i := range cameras {
		known = append(known, fmt.Sprintf("%s (%s)", cameras[i].ID, cameras[i].Name))
		if cameras[i].ID == camera {
			chosen = &cameras[i]
		}
	}
	if chosen == nil {
		return errResultf(errCodeInvalidArgument, "no healthy camera %q; healthy cameras: %s", camera, strings.Join(known, ", ")), nil
	}
	if label == "" {
		label = chosen.Name
	}
	view, err := m.start(ctx, conn, revision, watchSpec{CameraID: chosen.ID, CameraName: chosen.Name, Classes: classes, MinConfidence: confidence, Label: label})
	if err != nil {
		return watchErrorResult(err), nil
	}
	return okResult(map[string]any{"watch_id": view.WatchID, "label": view.Label, "state": view.State, "reason": view.Reason, "next_step": watchNextStep(view.State)}), nil
}

func watchNextStep(state string) string {
	switch watchState(state) {
	case watchReady:
		return "The watch is running. Events arrive as notifications; a client that does not show them can call watch_events with wait_seconds. Stop it with watch_stop when the user no longer needs it."
	case watchError:
		return "Tell the user the reason. The device keeps retrying; stop the watch with watch_stop if the user does not want to wait."
	case watchEnded:
		return "The watch ended; its reason says why."
	}
	return "The device is still preparing the detector; a first watch installs it, which takes minutes. READY arrives as a notification, or check with watch_list or watch_events."
}

func (s *mcpServer) handleWatchList(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	m := s.watchManager()
	if m == nil {
		return errResult(errCodeUnsupported, "watches are unavailable in this server"), nil
	}
	return okResult(map[string]any{"watches": m.list(), "free_slots": m.freeSlots()}), nil
}

func (s *mcpServer) handleWatchStop(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	m := s.watchManager()
	if m == nil {
		return errResult(errCodeUnsupported, "watches are unavailable in this server"), nil
	}
	id := strings.TrimSpace(req.GetString("watch_id", ""))
	if id == "" {
		return errResult(errCodeInvalidArgument, "watch_id is required"), nil
	}
	view, err := m.stop(ctx, id, watchStoppedReason)
	if errors.Is(err, errWatchNotFound) {
		return watchErrorResult(err), nil
	}
	result := map[string]any{"watch_id": view.WatchID, "label": view.Label, "state": view.State, "removed": err == nil}
	if err != nil {
		result["message"] = "The watch ended here, but removing its detector from the device failed (" + grpcErrString(err) + "); it stops by itself within a minute."
	}
	return okResult(result), nil
}

func (s *mcpServer) handleWatchEvents(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	m := s.watchManager()
	if m == nil {
		return errResult(errCodeUnsupported, "watches are unavailable in this server"), nil
	}
	id := strings.TrimSpace(req.GetString("watch_id", ""))
	if id == "" {
		return errResult(errCodeInvalidArgument, "watch_id is required"), nil
	}
	after := req.GetFloat("after_sequence", 0)
	wait := req.GetFloat("wait_seconds", 0)
	if math.IsNaN(after) || after < 0 || math.IsNaN(wait) || wait < 0 || wait > 120 {
		return errResult(errCodeInvalidArgument, "after_sequence must be at least 0 and wait_seconds from 0 to 120"), nil
	}
	view, events, gap, err := m.events(ctx, id, uint64(after), time.Duration(wait*float64(time.Second)))
	if err != nil {
		return watchErrorResult(err), nil
	}
	return okResult(map[string]any{"watch_id": view.WatchID, "label": view.Label, "state": view.State, "reason": view.Reason, "events": events, "gap": gap, "next_sequence": view.LastSequence}), nil
}

// startWatches gives the server its watch manager for the life of Start. The
// returned function removes every active watch when the server exits.
func (s *mcpServer) startWatches(srv *server.MCPServer) func() {
	notify := func(method string, params map[string]any) error {
		return srv.SendNotificationToSpecificClient("stdio", method, params)
	}
	m := newWatchManager(newCampaignWatchBackend(defaultWatchDetector), notify, func() uint64 {
		_, revision := s.connWithRevision()
		return revision
	})
	s.mu.Lock()
	s.watches = m
	s.mu.Unlock()
	return func() {
		m.shutdown()
		s.mu.Lock()
		if s.watches == m {
			s.watches = nil
		}
		s.mu.Unlock()
	}
}
```

- [ ] **Step 4: Wire the server**

In `server.go`:

(a) In `type mcpServer struct`, after `containerMCP *containerMCPManager`, add:

```go
	watches              *watchManager // guarded by mu; set while Start runs
```

(b) In `setConnectionLocked`, after the `if s.containerMCP != nil { … }` block, add:

```go
	if s.watches != nil {
		// Never call the manager with mu held: it reads the revision itself.
		go s.watches.endStale(s.connRevision)
	}
```

Note: `s.connRevision` is evaluated before the goroutine starts, so the goroutine receives the new revision.

(c) In `newProtocolServer`, add `s.registerWatchTools(srv)` directly before `registerToolAnalytics(srv)`.

(d) In `Start`, after `defer stopContainerMCP()`, add:

```go
	stopWatches := s.startWatches(srv)
	defer stopWatches() // runs first: removes watches while the connection is still open
```

In `tool_groups.go`, change the `hardware` entry to:

```go
	"hardware":      {"bluetooth_scan", "bluetooth_connect", "bluetooth_disconnect", "camera_list", "camera_controls", "camera_set_control", "camera_snapshot", "watch_sources", "watch_start", "watch_list", "watch_stop", "watch_events"},
```

In `tools_guide.go`, change the line `hardware: Bluetooth and cameras.` to `hardware: Bluetooth, cameras and camera watches.`, and add this section directly before `## Robot and sensor diagnostics`:

```
## Camera watches

A watch runs a detector on the connected device's camera and tells this
session when a watched class of object appears, for example a person at the
door. Nothing is recorded or uploaded. Call watch_sources for healthy cameras,
the detector's class labels and free slots (at most two watches per session),
then watch_start with a camera id and classes. It returns READY, ERROR, or
PREPARING after 30 s; a first watch on a device installs the detector, which
takes minutes. Events and status changes arrive as the notifications
notifications/wendy/watch_event and notifications/wendy/watch_status. A client
that does not show them can call watch_events with wait_seconds. ERROR is not
final: the device retries. Watches end with watch_stop, when the session ends,
or when the device connection changes; a watch whose device restarted ends
with that reason. An agent without leased campaigns reports UNSUPPORTED:
update it with `wendy device update`.

```

- [ ] **Step 5: Document**

In `go/internal/cli/assets/docs/integrations/mcp.mdx`, add a `## Camera watches` section after the section that lists tool groups (search for the heading that mentions tool groups; if none, add it before the error-codes section). Use this text:

```markdown
## Camera watches

The `hardware` group includes camera watches. A watch runs a detector on the
connected device's camera and tells the session when a watched class appears,
for example a person at the door. Nothing is recorded or uploaded, and watches
end when the MCP session ends or the device connection changes.

| Tool | What it does |
|---|---|
| `watch_sources` | Healthy cameras, the detector and its class labels, free slots |
| `watch_start` | Start a watch: `camera`, `classes` (1–10 labels), `min_confidence` (0.3–0.95, default 0.5), optional `label` |
| `watch_list` | This session's watches, including recently ended ones |
| `watch_stop` | Stop a watch and remove its detector from the device |
| `watch_events` | Buffered events after a sequence number; `wait_seconds` waits for the next |

At most two watches run per session. The server sends events and status changes
as `notifications/wendy/watch_event` and `notifications/wendy/watch_status`.
Clients that do not show custom notifications, such as Claude Code and Cursor,
use `watch_events`. Watches need an agent with leased campaigns; an older agent
reports `UNSUPPORTED` with the update command.
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd go && go test ./internal/cli/mcp/ -run 'Watch|CampaignPoller|DefaultWatchDetector|ToolGroups|Instructions|Guide' -race -v`
Expected: PASS, including the existing `TestToolGroupsProtocolDiscovery` (default count still 16, every built-in classified) and the instructions/guide tests.

Run: `cd go && go test ./internal/cli/mcp/`
Expected: `ok`. If `TestSkillsImportOverTransports` or another test fails, check whether it also fails on `ed/chat-watches-leased-campaigns` (memory notes it as failing main-wide); report it rather than changing it.

- [ ] **Step 7: Commit**

```bash
git add go/internal/cli/mcp/watch_tools.go go/internal/cli/mcp/watch_tools_test.go go/internal/cli/mcp/server.go go/internal/cli/mcp/tool_groups.go go/internal/cli/mcp/tools_guide.go go/internal/cli/assets/docs/integrations/mcp.mdx
git commit -m "feat(mcp): add camera watch tools

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M9Sy7Q2qsNmQy7v8wtkdUD"
```

---

### Task 6: Verify on the Orin Nano

**Files:** none unless a check fails.

- [ ] **Step 1: Local verification**

```bash
cd go
gofmt -l .
go vet ./internal/cli/mcp/
go test -race ./internal/cli/mcp/ -run 'Watch|CampaignPoller|DefaultWatchDetector'
go build ./...
GOOS=linux GOARCH=arm64 go build -o /dev/null ./cmd/wendy-agent/
```

Expected: `gofmt -l` prints nothing; the rest succeed.

- [ ] **Step 2: On-device check through the MCP server**

The device needs PR A's agent. Side-load it (`ed/chat-watches-leased-campaigns`), as PR A's Task 7 did: build `GOOS=linux GOARCH=arm64 go build -o "$SCRATCH/wendy-agent" ./cmd/wendy-agent`, record the current agent version with `wendy --json device info --device <dev>`, then run `wendy device update --device <dev> --binary "$SCRATCH/wendy-agent"`. Build this branch's CLI: `go build -o "$SCRATCH/wendy" ./cmd/wendy`.

Drive the server with a short script that speaks MCP over stdio to `"$SCRATCH/wendy" mcp serve --tool-groups all --device <dev>`. Write it in Go under the scratchpad, using mcp-go's client the way `go/internal/cli/chat/mcp.go` does, and register `OnNotification` to print notifications. It should:

1. call `watch_sources`, check the Brio is listed as `Brio 101`;
2. call `watch_start` with the Brio and `["person"]`; record the time to READY (a first start installs the runtime);
3. ask Ethan to step into view; check that a `notifications/wendy/watch_event` arrives with `kind: entered` and a `person` class, and that `watch_events` returns it;
4. call `watch_stop`; check with `wendy data campaign list` that the campaign is gone;
5. start a watch again, close the client (stdin), and check that the campaign is gone within 5 s;
6. start a watch, `kill -9` the script, and check that the campaign is gone within 90 s.

Expected: each step as described. Record the timings.

- [ ] **Step 3: Restore the device**

Put back the recorded agent version from its release binary (`gh release download <version> -R wendylabsinc/WendyOS -p 'wendy-agent-linux-arm64-<version>.tar.gz'` into an empty directory, extract, `wendy device update --device <dev> --binary <path>`). Then remove `/var/lib/wendy-agent/data/inference` if this test created it. Delete the scratch builds.
