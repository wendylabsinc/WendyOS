package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// watchDataClient is a small in-memory device that speaks data documents:
// stored documents, each inference's state, a notification journal and camera
// sources. calls records device operations in order, so tests can check, for
// example, journal-before-apply.
type watchDataClient struct {
	agentpbv2.DataServiceClient
	mu          sync.Mutex
	calls       []string
	documents   map[string]data.StoredDocument  // by "<kind>/<name>"
	raw         map[string][]byte               // the YAML each document was sent as
	inference   map[string]data.InferenceStatus // by inference name; default running
	renews      map[string]int
	inspects    int              // Documents calls, counted after the state is read
	applyErr    map[string]error // by kind
	applyWait   time.Duration    // DocumentApply waits this long, or until its ctx ends
	sourcesErr  error
	renewErr    error
	inspectErr  error
	removeErr   error
	removeWait  time.Duration
	journal     []data.CampaignNotification
	gapOnce     bool
	oldJournal  bool     // Events does not mark the response as the notification journal
	oversize    []byte   // when set, Events returns this as events_json once
	lastWatches []string // the watches the last Events call named
	sources     []*agentpbv2.DataSource
}

func newWatchDataClient() *watchDataClient {
	return &watchDataClient{
		documents: map[string]data.StoredDocument{}, raw: map[string][]byte{}, inference: map[string]data.InferenceStatus{}, renews: map[string]int{}, applyErr: map[string]error{},
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

func (f *watchDataClient) DocumentApply(ctx context.Context, r *agentpbv2.DataDocumentApplyRequest, _ ...grpc.CallOption) (*agentpbv2.DataDocumentApplyResponse, error) {
	f.mu.Lock()
	wait := f.applyWait
	f.mu.Unlock()
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	doc, err := data.ParseDocument(r.GetDocumentYaml())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ref := doc.Header.Kind + "/" + doc.Header.Name
	if err := f.applyErr[doc.Header.Kind]; err != nil {
		f.record("apply-error:" + ref)
		return nil, err
	}
	if doc.Header.Owner != "" {
		if _, ok := f.documents[doc.Header.Owner]; !ok {
			return nil, status.Errorf(codes.FailedPrecondition, "owner %s does not exist; apply it first", doc.Header.Owner)
		}
	}
	body, _ := json.Marshal(doc.Body)
	stored := data.StoredDocument{Kind: doc.Header.Kind, Version: doc.Header.Version, Name: doc.Header.Name, Owner: doc.Header.Owner, Revision: doc.Revision, Body: body}
	f.record("apply:" + ref)
	f.documents[ref] = stored
	f.raw[ref] = append([]byte(nil), r.GetDocumentYaml()...)
	return &agentpbv2.DataDocumentApplyResponse{Document: &agentpbv2.DataDocument{Kind: stored.Kind, Name: stored.Name, Owner: stored.Owner, Revision: stored.Revision, State: "waiting"}}, nil
}

func (f *watchDataClient) DocumentRenew(_ context.Context, r *agentpbv2.DataDocumentRenewRequest, _ ...grpc.CallOption) (*agentpbv2.DataDocumentRenewResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.renewErr != nil {
		return nil, f.renewErr
	}
	if _, ok := f.documents[r.GetKind()+"/"+r.GetName()]; !ok {
		return nil, status.Error(codes.NotFound, "document not found")
	}
	f.renews[r.GetName()]++
	return &agentpbv2.DataDocumentRenewResponse{ExpiresUnixNanos: time.Now().Add(time.Minute).UnixNano()}, nil
}

// DocumentRemove removes owned documents first, then the document, like the
// agent.
func (f *watchDataClient) DocumentRemove(ctx context.Context, r *agentpbv2.DataDocumentRemoveRequest, _ ...grpc.CallOption) (*agentpbv2.DataDocumentRemoveResponse, error) {
	f.mu.Lock()
	wait := f.removeWait
	f.mu.Unlock()
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ref := r.GetKind() + "/" + r.GetName()
	f.record("remove:" + ref)
	if f.removeErr != nil {
		return nil, f.removeErr
	}
	if _, ok := f.documents[ref]; !ok {
		return nil, status.Error(codes.NotFound, "document not found")
	}
	var removed []string
	for key, doc := range f.documents {
		if doc.Owner == ref {
			delete(f.documents, key)
			removed = append(removed, key)
		}
	}
	delete(f.documents, ref)
	return &agentpbv2.DataDocumentRemoveResponse{Removed: append(removed, ref)}, nil
}

// Documents reports a watch's state from its inference's, like the agent.
func (f *watchDataClient) Documents(_ context.Context, r *agentpbv2.DataDocumentsRequest, _ ...grpc.CallOption) (*agentpbv2.DataDocumentsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return nil, f.inspectErr
	}
	ref := r.GetKind() + "/" + r.GetName()
	stored, ok := f.documents[ref]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no document named %q", r.GetName())
	}
	document := &agentpbv2.DataDocument{Kind: stored.Kind, Name: stored.Name, Owner: stored.Owner, Revision: stored.Revision, DocumentJson: stored.Body}
	if stored.Kind == data.KindWatch {
		var watch data.Watch
		_ = json.Unmarshal(stored.Body, &watch)
		inference, ok := f.inference[watch.On.Inference]
		if !ok {
			inference = data.InferenceStatus{State: "running"}
		}
		if _, deployed := f.documents["inference/"+watch.On.Inference]; !deployed {
			inference = data.InferenceStatus{}
		}
		switch inference.State {
		case "running":
			document.State = "active"
		case "error":
			document.State, document.StateDetail = "error", inference.Error
		default:
			document.State = "waiting"
		}
		document.StatusJson, _ = json.Marshal(map[string]string{"inference_state": inference.State, "inference_error": inference.Error})
	}
	f.inspects++
	return &agentpbv2.DataDocumentsResponse{Documents: []*agentpbv2.DataDocument{document}}, nil
}

// Events serves the notification journal like the agent: an empty cursor
// without replay returns only the tail; a cursor returns entries after it,
// only of the named watches when watches is set.
func (f *watchDataClient) Events(_ context.Context, r *agentpbv2.DataEventsRequest, _ ...grpc.CallOption) (*agentpbv2.DataEventsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("events:" + r.GetCursor())
	f.lastWatches = append([]string(nil), r.GetWatches()...)
	var after uint64
	if _, number, ok := strings.Cut(r.GetCursor(), ":"); ok {
		after, _ = strconv.ParseUint(number, 10, 64)
	}
	out := []data.CampaignNotification{}
	var tail uint64
	for _, entry := range f.journal {
		tail = max(tail, entry.Sequence)
		if r.GetCursor() == "" || entry.Sequence <= after {
			continue
		}
		if len(r.GetWatches()) > 0 && !slices.Contains(r.GetWatches(), entry.Watch) {
			continue
		}
		out = append(out, entry)
	}
	raw, _ := json.Marshal(out)
	if f.oversize != nil && r.GetCursor() != "" {
		raw, f.oversize = f.oversize, nil
	}
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

// publish appends a leased watch's notification to the journal.
func (f *watchDataClient) publish(watch string, detections ...data.NotificationDetection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sequence := uint64(len(f.journal) + 1)
	f.journal = append(f.journal, data.CampaignNotification{
		ID: strconv.FormatUint(sequence, 10), Event: watch, Watch: watch, Inference: watch, Leased: true, Count: len(detections),
		Detections: detections, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano), Sequence: sequence,
	})
}

// publishThenOversize appends entries and arms an oversized response under one
// hold of the lock, so no poll can land between them.
func (f *watchDataClient) publishThenOversize(watch string, entries int, detection data.NotificationDetection, oversize []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for range entries {
		sequence := uint64(len(f.journal) + 1)
		f.journal = append(f.journal, data.CampaignNotification{
			ID: strconv.FormatUint(sequence, 10), Event: watch, Watch: watch, Inference: watch, Leased: true, Count: 1,
			Detections: []data.NotificationDetection{detection}, OccurredAt: time.Now().UTC().Format(time.RFC3339Nano), Sequence: sequence,
		})
	}
	f.oversize = oversize
}

func (f *watchDataClient) setInference(name string, state data.InferenceStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inference[name] = state
}

// setInferenceSeen sets an inference's state and waits until a Documents call
// has read it, so a watch polling the device cannot skip it.
func (f *watchDataClient) setInferenceSeen(t *testing.T, name string, state data.InferenceStatus) {
	t.Helper()
	f.mu.Lock()
	f.inference[name] = state
	seen := f.inspects
	f.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		n := f.inspects
		f.mu.Unlock()
		if n > seen {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no Documents call read inference state %+v", state)
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *watchDataClient) document(ref string) (data.StoredDocument, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	doc, ok := f.documents[ref]
	return doc, ok
}

func (f *watchDataClient) documentRefs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	refs := make([]string, 0, len(f.documents))
	for ref := range f.documents {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	return refs
}

func (f *watchDataClient) namedWatches() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lastWatches...)
}
