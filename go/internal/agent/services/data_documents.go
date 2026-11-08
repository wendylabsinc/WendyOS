package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/shared/datadoc"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// removalWait bounds how long DocumentRemove waits for removed inference
// jobs to stop (spec §9.3).
const removalWait = 10 * time.Second

// DocumentApply validates and stores a document and hands it to its runner
// (spec §9.2). A collection takes the CampaignDeploy path unchanged.
func (s *DataService) DocumentApply(ctx context.Context, req *agentpbv2.DataDocumentApplyRequest) (*agentpbv2.DataDocumentApplyResponse, error) {
	document, err := data.ParseDocument(req.GetDocumentYaml())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if document.Header.Kind == datadoc.DefaultKind {
		campaign, err := s.CampaignDeploy(ctx, &agentpbv2.DataCampaignDeployRequest{CampaignYaml: req.GetDocumentYaml()})
		if err != nil {
			return nil, err
		}
		return &agentpbv2.DataDocumentApplyResponse{Document: collectionMessage(campaign)}, nil
	}
	handler, ok := documentKindHandlers[document.Header.Kind]
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "this agent cannot run %s documents", document.Header.Kind)
	}
	if !handler.ready(s) {
		return nil, status.Error(codes.FailedPrecondition, "agent inference and watch runtime is unavailable")
	}
	stored, err := s.applyDocument(document)
	if err != nil {
		return nil, err
	}
	handler.applied(s)
	return &agentpbv2.DataDocumentApplyResponse{Document: s.documentMessage(stored)}, nil
}

func (s *DataService) applyDocument(document datadoc.Document) (data.StoredDocument, error) {
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	header := document.Header
	if header.Owner != "" {
		ownerKind, ownerName, _ := strings.Cut(header.Owner, "/")
		owner, err := s.manager.Document(ownerKind, ownerName)
		if errors.Is(err, os.ErrNotExist) {
			return data.StoredDocument{}, status.Errorf(codes.FailedPrecondition, "owner %s does not exist; apply it first", header.Owner)
		}
		if err != nil {
			return data.StoredDocument{}, dataStatusError(err)
		}
		if owner.Owner != "" {
			return data.StoredDocument{}, status.Errorf(codes.FailedPrecondition, "owner %s is itself owned by %s; ownership is one level deep", header.Owner, owner.Owner)
		}
	}
	existing, err := s.manager.Document(header.Kind, header.Name)
	switch {
	case err == nil:
		if existing.Owner != header.Owner {
			return data.StoredDocument{}, status.Errorf(codes.FailedPrecondition, "%s has owner %q; a redeploy cannot change it", existing.Ref(), existing.Owner)
		}
		if watch, ok := document.Body.(data.Watch); ok {
			previous, err := existing.Watch()
			if err != nil {
				return data.StoredDocument{}, status.Error(codes.Internal, err.Error())
			}
			if previous.Leased() != watch.Leased() {
				return data.StoredDocument{}, status.Errorf(codes.FailedPrecondition, "%s: a redeploy cannot add or drop a lease", existing.Ref())
			}
		}
	case !errors.Is(err, os.ErrNotExist):
		return data.StoredDocument{}, dataStatusError(err)
	}
	stored, err := s.manager.SaveDocument(document)
	if err != nil {
		return data.StoredDocument{}, dataStatusError(err)
	}
	// Applying or reapplying a leased watch starts its lease afresh.
	if watch, ok := document.Body.(data.Watch); ok && watch.Leased() {
		s.leases.extend(watch.Name, watch.LeaseDuration())
	}
	return stored, nil
}

// DocumentRemove removes an inference or watch document and what it owns,
// then waits, at most removalWait, for their inference to stop.
func (s *DataService) DocumentRemove(ctx context.Context, req *agentpbv2.DataDocumentRemoveRequest) (*agentpbv2.DataDocumentRemoveResponse, error) {
	if req.GetKind() == datadoc.DefaultKind {
		return nil, status.Error(codes.FailedPrecondition, "remove a collection with `wendy data campaign remove`")
	}
	if !slices.Contains(data.StoredKinds(), req.GetKind()) {
		return nil, status.Errorf(codes.InvalidArgument, "unknown document kind %q", req.GetKind())
	}
	removed, err := s.removeDocumentTree(req.GetKind(), req.GetName())
	if err != nil {
		return nil, dataStatusError(err)
	}
	if err := s.waitForRetiredJobs(ctx); err != nil {
		return nil, err
	}
	return &agentpbv2.DataDocumentRemoveResponse{Removed: removed}, nil
}

// waitForRetiredJobs runs a reconcile pass and waits for it, which returns
// once every job without a document has stopped. Callers must not hold
// deploymentMu: a retiring campaign job may need it inside triggerInference,
// so reconciling under the lock would deadlock.
func (s *DataService) waitForRetiredJobs(ctx context.Context) error {
	if s.inference == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, removalWait)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.inference.reconcileNow() }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return status.Error(codes.DeadlineExceeded, "the documents are removed; their inference is still stopping")
	}
}

// DocumentRenew pushes a leased watch's deadline to now + lease. It takes no
// deploymentMu: a capture start holding it must not make an on-time renewal
// late.
func (s *DataService) DocumentRenew(_ context.Context, req *agentpbv2.DataDocumentRenewRequest) (*agentpbv2.DataDocumentRenewResponse, error) {
	stored, err := s.manager.Document(req.GetKind(), req.GetName())
	if err != nil {
		return nil, dataStatusError(err)
	}
	if stored.Kind != data.KindWatch {
		return nil, status.Errorf(codes.FailedPrecondition, "%s has no lease", stored.Ref())
	}
	watch, err := stored.Watch()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !watch.Leased() {
		return nil, status.Errorf(codes.FailedPrecondition, "%s has no lease", stored.Ref())
	}
	deadline, ok := s.leases.renew(watch.Name, watch.LeaseDuration())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "%s: its lease expired", stored.Ref())
	}
	return &agentpbv2.DataDocumentRenewResponse{ExpiresUnixNanos: deadline.UnixNano()}, nil
}

// Documents lists documents of every kind, or one, with their status.
func (s *DataService) Documents(_ context.Context, req *agentpbv2.DataDocumentsRequest) (*agentpbv2.DataDocumentsResponse, error) {
	kinds := data.DocumentKinds()
	if req.GetKind() != "" {
		if !slices.Contains(kinds, req.GetKind()) {
			return nil, status.Errorf(codes.InvalidArgument, "unknown document kind %q", req.GetKind())
		}
		kinds = []string{req.GetKind()}
	}
	var documents []*agentpbv2.DataDocument
	for _, kind := range kinds {
		if kind == datadoc.DefaultKind {
			campaigns, err := s.manager.Campaigns()
			if err != nil {
				return nil, dataStatusError(err)
			}
			for _, campaign := range campaigns {
				if req.GetName() != "" && campaign.Name != req.GetName() {
					continue
				}
				message, err := s.campaignMessage(campaign)
				if err != nil {
					return nil, status.Error(codes.Internal, err.Error())
				}
				documents = append(documents, collectionMessage(message))
			}
			continue
		}
		stored, err := s.manager.Documents(kind)
		if err != nil {
			return nil, dataStatusError(err)
		}
		for _, document := range stored {
			if req.GetName() == "" || document.Name == req.GetName() {
				documents = append(documents, s.documentMessage(document))
			}
		}
	}
	if req.GetName() != "" && len(documents) == 0 {
		return nil, status.Errorf(codes.NotFound, "no document named %q", req.GetName())
	}
	return &agentpbv2.DataDocumentsResponse{Documents: documents}, nil
}

// collectionMessage presents a campaign as a collection document.
func collectionMessage(campaign *agentpbv2.DataCampaign) *agentpbv2.DataDocument {
	return &agentpbv2.DataDocument{Kind: datadoc.DefaultKind, Version: uint32(data.CampaignVersion), Name: campaign.GetName(), Revision: campaign.GetRevision(),
		State: campaign.GetState(), DeployedUnixNanos: campaign.GetDeployedUnixNanos(), DocumentJson: campaign.GetPlanJson(), Warnings: campaign.GetWarnings()}
}

// documentKindHandler is what the service does with one stored kind beyond
// storing, listing and removing it, which follow the data registry.
type documentKindHandler struct {
	// ready reports whether the kind's runner can take a document.
	ready func(s *DataService) bool
	// applied tells the kind's runner that a document was stored.
	applied func(s *DataService)
	// status fills in a stored document's state and status (spec §6.6).
	status func(s *DataService, stored data.StoredDocument, message *agentpbv2.DataDocument)
}

// documentKindHandlers has one entry per kind in data.StoredKinds;
// TestEveryStoredKindHasAHandler fails when a registered kind has none. The
// inference loop runs inference documents (campaignInferenceManager.reconcile)
// and startWatches runs watches.
var documentKindHandlers = map[string]documentKindHandler{
	data.KindInference: {
		// A watch on the document needs the watch runtime too.
		ready:   func(s *DataService) bool { return s.inference != nil && s.watches != nil && s.video != nil },
		applied: func(s *DataService) { s.inference.wakeNow() },
		status:  (*DataService).inferenceDocumentStatus,
	},
	data.KindWatch: {
		ready: func(s *DataService) bool { return s.inference != nil && s.watches != nil },
		// The watch runner reads the stored watches on every pass.
		applied: func(*DataService) {},
		status:  (*DataService).watchDocumentStatus,
	},
}

// documentMessage presents a stored document with its status.
func (s *DataService) documentMessage(stored data.StoredDocument) *agentpbv2.DataDocument {
	message := &agentpbv2.DataDocument{Kind: stored.Kind, Version: uint32(stored.Version), Name: stored.Name, Owner: stored.Owner, Revision: stored.Revision,
		DeployedUnixNanos: stored.DeployedUnixNanos, DocumentJson: stored.Body, Warnings: stored.Warnings}
	if handler, ok := documentKindHandlers[stored.Kind]; ok {
		handler.status(s, stored, message)
	}
	return message
}

func (s *DataService) inferenceDocumentStatus(stored data.StoredDocument, message *agentpbv2.DataDocument) {
	snapshot := s.inferenceStatus(stored)
	message.State, message.StateDetail = snapshot.State, snapshot.Error
	message.StatusJson, _ = json.Marshal(snapshot)
}

func (s *DataService) watchDocumentStatus(stored data.StoredDocument, message *agentpbv2.DataDocument) {
	watch, err := stored.Watch()
	if err != nil {
		message.State, message.StateDetail = "error", err.Error()
		return
	}
	state, detail, inference := s.watchState(watch)
	message.State, message.StateDetail = state, detail
	if watch.Leased() {
		if deadline, ok := s.leases.deadline(watch.Name); ok {
			message.ExpiresUnixNanos = deadline.UnixNano()
		}
	}
	deliveryError := ""
	if s.watches != nil {
		deliveryError = s.watches.deliveryError(watch.Name)
	}
	message.StatusJson, _ = json.Marshal(map[string]string{"inference_state": inference.State, "inference_error": inference.Error, "delivery_error": deliveryError})
}

// inferenceStatus is an inference document's status: disabled when it is not
// enabled, error when the runtime is missing, otherwise its job's status, with
// a job that does not exist yet reported as pending.
func (s *DataService) inferenceStatus(stored data.StoredDocument) data.InferenceStatus {
	document, err := stored.Inference()
	if err != nil {
		return data.InferenceStatus{State: "error", Error: err.Error()}
	}
	if !document.IsEnabled() {
		return data.InferenceStatus{State: "disabled"}
	}
	if s.inference == nil {
		return data.InferenceStatus{State: "error", Error: "agent inference runtime is unavailable"}
	}
	snapshot := *s.inference.snapshot(inferenceJobKey(stored.Name), stored.Revision)
	if snapshot.State == "stopped" {
		snapshot.State = "pending"
	}
	return snapshot
}

// watchState maps a watch's inference to waiting, active or error, and
// returns the inference's own status for the watch's status_json, so a client
// can tell a camera that is unavailable from a detector still loading.
func (s *DataService) watchState(watch data.Watch) (state, detail string, inference data.InferenceStatus) {
	stored, err := s.manager.Document(data.KindInference, watch.On.Inference)
	if err != nil {
		return "waiting", "inference " + watch.On.Inference + " is not deployed", data.InferenceStatus{}
	}
	snapshot := s.inferenceStatus(stored)
	if snapshot.State == "disabled" {
		return "waiting", "inference " + watch.On.Inference + " is disabled", snapshot
	}
	switch snapshot.State {
	case "running":
		return "active", "", snapshot
	case "error":
		return "error", snapshot.Error, snapshot
	}
	return "waiting", "inference " + watch.On.Inference + " is " + snapshot.State, snapshot
}
