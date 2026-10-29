package mcp

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
)

func fastDocumentBackend() *documentWatchBackend {
	b := newDocumentWatchBackend()
	b.renewEvery, b.statusEvery, b.pollEvery = 20*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond
	return b
}

func testWatchSpec(name string) watchSpec {
	return watchSpec{Name: name, CameraID: "v4l2:/dev/video0", CameraName: "Brio 101", Classes: []string{"person"}, MinConfidence: 0.5, Label: "door", Detector: watchDetectors[0]}
}

func startDocumentTestWatch(t *testing.T, b *documentWatchBackend, f *watchDataClient, name string) watchHandle {
	t.Helper()
	h, err := b.Start(context.Background(), &grpcclient.AgentConnection{DataService: f}, testWatchSpec(name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	return h
}

func nextUpdate(t *testing.T, h watchHandle) watchUpdate {
	t.Helper()
	select {
	case u, ok := <-h.Updates():
		if !ok {
			t.Fatal("updates closed")
		}
		return u
	case <-time.After(3 * time.Second):
		t.Fatal("no update")
	}
	return watchUpdate{}
}

func nextStatus(t *testing.T, h watchHandle) watchStatusUpdate {
	t.Helper()
	for {
		if u := nextUpdate(t, h); u.Status != nil {
			return *u.Status
		}
	}
}

func nextEvent(t *testing.T, h watchHandle) watchUpdate {
	t.Helper()
	for {
		if u := nextUpdate(t, h); u.Event != nil || u.Gap != "" {
			return u
		}
	}
}

func TestDocumentWatchAppliesLeasedWatchAndOwnedInference(t *testing.T) {
	f := newWatchDataClient()
	startDocumentTestWatch(t, fastDocumentBackend(), f, "chat-a-1")
	calls := f.callLog()
	applies := slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return !strings.HasPrefix(c, "apply:") })
	if strings.Join(applies, ",") != "apply:watch/chat-a-1,apply:inference/chat-a-1" {
		t.Fatalf("applies = %v; the watch must come first", applies)
	}
	if !strings.HasPrefix(calls[0], "events:") {
		t.Fatalf("calls = %v; the journal tail must be read before applying", calls)
	}
	watchDoc, _ := f.document("watch/chat-a-1")
	inferenceDoc, _ := f.document("inference/chat-a-1")
	watch, err := watchDoc.Watch()
	if err != nil || watch.Lease != "60s" || watch.On.Inference != "chat-a-1" || len(watch.Notify) != 0 {
		t.Fatalf("watch = %+v, %v", watch, err)
	}
	inference, err := inferenceDoc.Inference()
	if err != nil || inferenceDoc.Owner != "watch/chat-a-1" || inference.Sources[0].Camera != "v4l2:/dev/video0" ||
		inference.Model != "ustc-community/dfine-nano-coco" || inference.Revision != "066438d3d8f0da137a37b38fdf3368fd4afceced" ||
		!slices.Equal(inference.Labels, []string{"person"}) || inference.Threshold != 0.5 || inference.Rate != 2 ||
		inference.ClearAfter != "5s" || inference.Cooldown != "30s" || inference.Event != "chat-a-1" {
		t.Fatalf("inference = %+v (owner %q), %v", inference, inferenceDoc.Owner, err)
	}
}

func TestDocumentWatchSendsOnlyTheDocumentKeys(t *testing.T) {
	watchYAML, inferenceYAML, err := watchDocuments(testWatchSpec("chat-a-1"))
	if err != nil {
		t.Fatal(err)
	}
	keys := func(raw []byte) string {
		var m map[string]any
		if err := yaml.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		var out []string
		for k := range m {
			out = append(out, k)
		}
		slices.Sort(out)
		return strings.Join(out, ",")
	}
	if got := keys(watchYAML); got != "kind,lease,name,on,version" {
		t.Fatalf("watch keys = %s", got)
	}
	if got := keys(inferenceYAML); got != "clear_after,cooldown,kind,labels,model,name,owner,rate,revision,sources,threshold,version" {
		t.Fatalf("inference keys = %s", got)
	}
}

func TestDocumentWatchApplyHasADeadline(t *testing.T) {
	f := newWatchDataClient()
	f.applyWait = time.Hour
	b := fastDocumentBackend()
	b.deployTimeout = 50 * time.Millisecond
	_, err := b.Start(context.Background(), &grpcclient.AgentConnection{DataService: f}, testWatchSpec("chat-a-1"))
	var unconfirmed watchUnconfirmedError
	if !errors.As(err, &unconfirmed) || !errors.Is(err, errWatchDeviceBusy) {
		t.Fatalf("error = %v; want an unconfirmed busy device", err)
	}
}

func TestDocumentWatchApplyCancelledByTheCallerIsNotBusy(t *testing.T) {
	f := newWatchDataClient()
	f.applyWait = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	_, err := fastDocumentBackend().Start(ctx, &grpcclient.AgentConnection{DataService: f}, testWatchSpec("chat-a-1"))
	if errors.Is(err, errWatchDeviceBusy) || !errors.As(err, new(watchUnconfirmedError)) {
		t.Fatalf("error = %v; want unconfirmed but not busy", err)
	}
}

func TestDocumentWatchRemovesTheWatchWhenTheInferenceFails(t *testing.T) {
	f := newWatchDataClient()
	f.applyErr[data.KindInference] = status.Error(codes.InvalidArgument, "inference.labels must contain 1..32 labels")
	b := fastDocumentBackend()
	_, err := b.Start(context.Background(), &grpcclient.AgentConnection{DataService: f}, testWatchSpec("chat-a-1"))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("error = %v", err)
	}
	if refs := f.documentRefs(); len(refs) != 0 {
		t.Fatalf("left on the device: %v", refs)
	}
	if !slices.Contains(f.callLog(), "remove:watch/chat-a-1") {
		t.Fatal("the watch was not removed")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.poller != nil || len(b.pending) != 0 {
		t.Fatal("the failed start kept the poller or its pending name")
	}
}

func TestDocumentWatchOldAgentRefusesCleanly(t *testing.T) {
	for name, set := range map[string]func(*watchDataClient){
		"no DocumentApply": func(f *watchDataClient) {
			f.applyErr[data.KindWatch] = status.Error(codes.Unimplemented, "unknown method DocumentApply")
		},
		"old journal": func(f *watchDataClient) { f.oldJournal = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newWatchDataClient()
			set(f)
			_, err := fastDocumentBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: f}, testWatchSpec("chat-a-1"))
			if !errors.Is(err, errWatchAgentTooOld) {
				t.Fatalf("error = %v", err)
			}
			if refs := f.documentRefs(); len(refs) != 0 {
				t.Fatalf("left on the device: %v", refs)
			}
		})
	}
}

func TestDocumentWatchMapsTheWatchState(t *testing.T) {
	f := newWatchDataClient()
	f.setInference("chat-a-1", data.InferenceStatus{State: "loading"})
	h := startDocumentTestWatch(t, fastDocumentBackend(), f, "chat-a-1")
	if s := nextStatus(t, h); s.State != watchPreparing || !strings.Contains(s.Reason, "loading the detector") {
		t.Fatalf("loading = %+v", s)
	}
	f.setInference("chat-a-1", data.InferenceStatus{State: "waiting_for_cameras", Error: "camera unplugged"})
	if s := nextStatus(t, h); s.State != watchError || !strings.Contains(s.Reason, "camera unplugged") {
		t.Fatalf("camera gone = %+v", s)
	}
	f.setInference("chat-a-1", data.InferenceStatus{State: "running"})
	if s := nextStatus(t, h); s.State != watchReady {
		t.Fatalf("running = %+v", s)
	}
	f.setInference("chat-a-1", data.InferenceStatus{State: "error", Error: "inference: ValueError: bad"})
	if s := nextStatus(t, h); s.State != watchError || s.Reason != "inference: ValueError: bad" {
		t.Fatalf("error = %+v", s)
	}
}

// The agent retries a failed detector by loading it again every 5 s. The
// retry belongs to ERROR: a watch must not flap between ERROR and PREPARING.
func TestDocumentWatchStaysInErrorWhileTheAgentRetries(t *testing.T) {
	f := newWatchDataClient()
	failed := data.InferenceStatus{State: "error", Error: "model download failed"}
	f.setInference("chat-a-1", failed)
	h := startDocumentTestWatch(t, fastDocumentBackend(), f, "chat-a-1")
	got := []watchStatusUpdate{nextStatus(t, h)}
	for range 4 {
		f.setInferenceSeen(t, "chat-a-1", data.InferenceStatus{State: "loading"})
		f.setInferenceSeen(t, "chat-a-1", failed)
		f.setInferenceSeen(t, "chat-a-1", data.InferenceStatus{State: "pending"})
	}
	f.setInferenceSeen(t, "chat-a-1", data.InferenceStatus{State: "error", Error: "out of memory"})
	f.setInferenceSeen(t, "chat-a-1", data.InferenceStatus{State: "loading"})
	f.setInference("chat-a-1", data.InferenceStatus{State: "running"})
	for {
		s := nextStatus(t, h)
		got = append(got, s)
		if s.State == watchReady {
			break
		}
	}
	want := []watchStatusUpdate{{State: watchError, Reason: "model download failed"}, {State: watchError, Reason: "out of memory"}, {State: watchReady}}
	if !slices.Equal(got, want) {
		t.Fatalf("statuses %+v, want %+v", got, want)
	}
}

func TestDocumentWatchRenewsAndEndsWhenTheLeaseIsGone(t *testing.T) {
	f := newWatchDataClient()
	h := startDocumentTestWatch(t, fastDocumentBackend(), f, "chat-a-1")
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		renews := f.renews["chat-a-1"]
		f.mu.Unlock()
		if renews >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the watch was not renewed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.mu.Lock()
	delete(f.documents, "watch/chat-a-1")
	f.mu.Unlock()
	for {
		s := nextStatus(t, h)
		if s.State == watchEnded {
			if s.Reason != watchGoneReason {
				t.Fatalf("reason = %q", s.Reason)
			}
			break
		}
	}
}

func TestDocumentWatchStopRemovesTheWatchAndItsInference(t *testing.T) {
	f := newWatchDataClient()
	h, err := fastDocumentBackend().Start(context.Background(), &grpcclient.AgentConnection{DataService: f}, testWatchSpec("chat-a-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if refs := f.documentRefs(); len(refs) != 0 {
		t.Fatalf("left on the device: %v", refs)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("a second stop (NOT_FOUND) = %v", err)
	}
}

func TestDocumentPollerReadsOnlyItsOwnWatches(t *testing.T) {
	f := newWatchDataClient()
	b := fastDocumentBackend()
	h := startDocumentTestWatch(t, b, f, "chat-a-1")
	f.publish("chat-b-1", data.NotificationDetection{Label: "dog", Score: 0.8})
	f.publish("chat-a-1", data.NotificationDetection{Label: "person", Score: 0.9})
	u := nextEvent(t, h)
	if u.Event == nil || len(u.Event.Classes) != 1 || u.Event.Classes[0].Label != "person" {
		t.Fatalf("event = %+v; another session's watch leaked in", u)
	}
	if named := f.namedWatches(); !slices.Equal(named, []string{"chat-a-1"}) {
		t.Fatalf("the poller named %v", named)
	}
}

func TestDocumentPollerReportsGapsToEveryWatch(t *testing.T) {
	f := newWatchDataClient()
	b := fastDocumentBackend()
	first := startDocumentTestWatch(t, b, f, "chat-a-1")
	second := startDocumentTestWatch(t, b, f, "chat-a-2")
	f.mu.Lock()
	f.gapOnce = true
	f.mu.Unlock()
	for _, h := range []watchHandle{first, second} {
		if u := nextEvent(t, h); u.Gap == "" {
			t.Fatalf("update = %+v; want a gap", u)
		}
	}
}

func TestDocumentPollerCapsAResponse(t *testing.T) {
	f := newWatchDataClient()
	b := fastDocumentBackend()
	h := startDocumentTestWatch(t, b, f, "chat-a-1")
	// These entries sit before the oversized response's cursor, so the poller
	// must have moved past them: they are never delivered afterwards.
	f.publishThenOversize("chat-a-1", 3, data.NotificationDetection{Label: "stale", Score: 0.9},
		[]byte(`[`+strings.Repeat(`{"watch":"chat-a-1"},`, 220_000)+`{}]`))
	if u := nextEvent(t, h); u.Gap == "" {
		t.Fatalf("update = %+v; an oversized response must become a gap", u)
	}
	f.publish("chat-a-1", data.NotificationDetection{Label: "person", Score: 0.9})
	u := nextEvent(t, h)
	if u.Event == nil || u.Event.Classes[0].Label != "person" {
		t.Fatalf("update = %+v; the poller must move on after the cap, past the entries it skipped", u)
	}
}

func TestDocumentPollerKeepsTheLast512Entries(t *testing.T) {
	f := newWatchDataClient()
	h := startDocumentTestWatch(t, fastDocumentBackend(), f, "chat-a-1")
	f.mu.Lock()
	for i := 1; i <= 513; i++ {
		sequence := uint64(i)
		f.journal = append(f.journal, data.CampaignNotification{
			ID: strconv.Itoa(i), Watch: "chat-a-1", Leased: true, Count: 1, Sequence: sequence,
			Detections: []data.NotificationDetection{{Label: "n" + strconv.Itoa(i), Score: 0.9}},
			OccurredAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	}
	f.mu.Unlock()
	if u := nextEvent(t, h); u.Gap == "" {
		t.Fatalf("update = %+v; 513 entries must report a gap first", u)
	}
	var labels []string
	for len(labels) < 512 {
		u := nextUpdate(t, h)
		if u.Event != nil {
			labels = append(labels, u.Event.Classes[0].Label)
		}
	}
	if labels[0] != "n2" || labels[511] != "n513" {
		t.Fatalf("kept %s..%s; want the last 512 (n2..n513)", labels[0], labels[511])
	}
	select {
	case u := <-h.Updates():
		if u.Event != nil {
			t.Fatalf("extra event %+v after the last 512", u.Event)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

// Stopping the last running watch must not stop the poller while another
// start is still applying its documents.
func TestDocumentPollerKeepsRunningForAStartInFlight(t *testing.T) {
	f := newWatchDataClient()
	b := fastDocumentBackend()
	conn := &grpcclient.AgentConnection{DataService: f}
	a, err := b.Start(context.Background(), conn, testWatchSpec("chat-a-1"))
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.applyWait = 150 * time.Millisecond
	f.mu.Unlock()
	type result struct {
		h   watchHandle
		err error
	}
	started := make(chan result, 1)
	go func() {
		h, err := b.Start(context.Background(), conn, testWatchSpec("chat-a-2"))
		started <- result{h, err}
	}()
	time.Sleep(30 * time.Millisecond) // the second start is inside its apply
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := <-started
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { _ = r.h.Stop(context.Background()) })
	f.publish("chat-a-2", data.NotificationDetection{Label: "dog", Score: 0.8})
	if u := nextEvent(t, r.h); u.Event == nil || u.Event.Classes[0].Label != "dog" {
		t.Fatalf("got %+v", u)
	}
}

func TestDocumentPollerStopsWithTheLastWatch(t *testing.T) {
	f := newWatchDataClient()
	b := fastDocumentBackend()
	h, err := b.Start(context.Background(), &grpcclient.AgentConnection{DataService: f}, testWatchSpec("chat-a-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.poller != nil {
		t.Fatal("the poller outlived the last watch")
	}
}

func TestDocumentPollerConcurrentStartsShareOnePoller(t *testing.T) {
	f := newWatchDataClient()
	b := fastDocumentBackend()
	var wg sync.WaitGroup
	for i := 1; i <= 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			startDocumentTestWatch(t, b, f, "chat-a-"+string(rune('0'+i)))
		}(i)
	}
	wg.Wait()
	tails := 0
	for _, call := range f.callLog() {
		if call == "events:" {
			tails++
		}
	}
	if tails != 1 {
		t.Fatalf("%d tail reads; concurrent starts must share one poller", tails)
	}
}

func TestWatchStateFromInferenceLoadingReasonFitsAWarmDevice(t *testing.T) {
	state, reason := watchStateFromInference(data.InferenceStatus{State: "loading"})
	if state != watchPreparing || reason != "loading the detector; a first watch on a device also installs it, which takes a few minutes" {
		t.Fatalf("loading -> %s %q", state, reason)
	}
}

// The agent applies documents under the lock a recording campaign holds while
// it starts a capture, which can take over 20 s, so an apply gets longer than
// the other device calls.
func TestDocumentWatchApplyOutlastsTheCallTimeout(t *testing.T) {
	if b := newDocumentWatchBackend(); b.deployTimeout != 30*time.Second || b.callTimeout != 10*time.Second {
		t.Fatalf("deploy timeout %s, call timeout %s", b.deployTimeout, b.callTimeout)
	}
	f := newWatchDataClient()
	f.applyWait = 150 * time.Millisecond
	b := fastDocumentBackend()
	b.callTimeout, b.deployTimeout = 50*time.Millisecond, 2*time.Second
	h, err := b.Start(context.Background(), &grpcclient.AgentConnection{DataService: f}, testWatchSpec("chat-a-1"))
	if err != nil {
		t.Fatalf("an apply within its own timeout failed: %v", err)
	}
	_ = h.Stop(context.Background())
}

// An apply that ended without the device's answer may still land; a refusal
// did not.
func TestDocumentWatchApplyFailuresSayWhetherTheyMayLand(t *testing.T) {
	cases := []struct {
		name     string
		applyErr error
		timeout  bool
		mayLand  bool
	}{
		{name: "busy", timeout: true, mayLand: true},
		{name: "unreachable", applyErr: status.Error(codes.Unavailable, "connection reset"), mayLand: true},
		{name: "cancelled", applyErr: status.Error(codes.Canceled, "context canceled"), mayLand: true},
		{name: "refused", applyErr: status.Error(codes.FailedPrecondition, "agent inference runtime is unavailable")},
		{name: "old agent", applyErr: status.Error(codes.Unimplemented, "unknown method")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWatchDataClient()
			f.applyErr[data.KindWatch] = tc.applyErr
			b := fastDocumentBackend()
			if tc.timeout {
				f.applyWait, b.deployTimeout = 5*time.Second, 50*time.Millisecond
			}
			h, err := b.Start(context.Background(), &grpcclient.AgentConnection{DataService: f}, testWatchSpec("chat-a-1"))
			if err == nil {
				_ = h.Stop(context.Background())
				t.Fatal("the apply must fail")
			}
			var unconfirmed watchUnconfirmedError
			if errors.As(err, &unconfirmed) != tc.mayLand {
				t.Fatalf("%v: may land %v, want %v", err, !tc.mayLand, tc.mayLand)
			}
			if tc.timeout && !errors.Is(err, errWatchDeviceBusy) {
				t.Fatalf("a busy device must still read as busy: %v", err)
			}
			if tc.applyErr != nil && status.Code(err) != status.Code(tc.applyErr) && !errors.Is(err, errWatchAgentTooOld) {
				t.Fatalf("lost the device's code: %v", err)
			}
			if tc.mayLand && tc.applyErr != nil && grpcErrString(err) != grpcErrString(tc.applyErr) {
				t.Fatalf("tools would report %q, not the device's %q", grpcErrString(err), grpcErrString(tc.applyErr))
			}
			if !tc.mayLand {
				if refs := f.documentRefs(); len(refs) != 0 {
					t.Fatalf("left on the device: %v", refs)
				}
			}
		})
	}
}

// Two watches share one poller; an entry reaches only the watch it names.
func TestDocumentPollerRoutesAnEntryToItsOwnWatch(t *testing.T) {
	f := newWatchDataClient()
	b := fastDocumentBackend()
	front := startDocumentTestWatch(t, b, f, "chat-a-1")
	back := startDocumentTestWatch(t, b, f, "chat-a-2")
	expectNothing := func(h watchHandle, label string) {
		t.Helper()
		deadline := time.After(150 * time.Millisecond)
		for {
			select {
			case u := <-h.Updates():
				if u.Event != nil || u.Gap != "" {
					t.Fatalf("%s: got %+v for another watch's entry", label, u)
				}
			case <-deadline:
				return
			}
		}
	}
	for _, tc := range []struct {
		target, other watchHandle
		name, label   string
	}{{back, front, "chat-a-2", "dog"}, {front, back, "chat-a-1", "person"}} {
		f.publish(tc.name, data.NotificationDetection{Label: tc.label, Score: 0.9})
		if u := nextEvent(t, tc.target); u.Event == nil || u.Event.Classes[0].Label != tc.label {
			t.Fatalf("%s: event = %+v", tc.name, u)
		}
		expectNothing(tc.other, "the other watch after "+tc.name)
	}
}

// The lease starts when the agent applies the watch, which may be long before
// Start returns on a busy device, so the first renewal cannot wait a full
// interval.
func TestDocumentWatchRenewsRightAfterItStarts(t *testing.T) {
	f := newWatchDataClient()
	b := fastDocumentBackend()
	b.renewEvery = time.Hour
	startDocumentTestWatch(t, b, f, "chat-a-1")
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		renews := f.renews["chat-a-1"]
		f.mu.Unlock()
		if renews >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no renewal after Start returned")
		}
		time.Sleep(time.Millisecond)
	}
}

// A waiting watch whose inference is gone or disabled (for example after
// `wendy data remove inference/chat-...`) says why the device is waiting.
func TestWatchStateFromDocumentSaysWhyAWaitingWatchWaits(t *testing.T) {
	const detail = "waiting for inference/chat-a-1"
	for _, inferenceState := range []string{"", "disabled"} {
		document := &agentpbv2.DataDocument{Kind: data.KindWatch, State: "waiting", StateDetail: detail}
		document.StatusJson = []byte(`{"inference_state":"` + inferenceState + `"}`)
		if state, reason := watchStateFromDocument(document); state != watchPreparing || reason != detail {
			t.Errorf("inference %q -> %s %q; want PREPARING with the device's detail", inferenceState, state, reason)
		}
	}
	// An inference state that maps on its own keeps its own reason.
	loading := &agentpbv2.DataDocument{Kind: data.KindWatch, State: "waiting", StateDetail: detail, StatusJson: []byte(`{"inference_state":"loading"}`)}
	if _, reason := watchStateFromDocument(loading); !strings.Contains(reason, "loading the detector") {
		t.Errorf("loading reason = %q", reason)
	}
}
