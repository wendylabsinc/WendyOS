package node

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"math"
	"net/http"
)

type TasksResponse struct {
	Tasks []tasking.Record `json:"tasks"`
}
type StopRequest struct {
	TaskID string `json:"task_id"`
}
type EvaluateRequest struct {
	TaskID        string            `json:"task_id"`
	ExecutorEpoch uint64            `json:"executor_epoch"`
	Telemetry     tasking.Telemetry `json:"telemetry"`
}
type ReportRequest struct {
	TaskID        string        `json:"task_id"`
	ExecutorEpoch uint64        `json:"executor_epoch"`
	Version       uint64        `json:"version"`
	Phase         mission.Phase `json:"phase"`
}
type ExecutorStartResponse struct {
	ExecutorEpoch uint64 `json:"executor_epoch"`
}

// MissionAuthority rechecks the stored, immutable operator signature and current
// revocations before the task store allows any continued execution or report.
func MissionAuthority(v *authority.Verifier) func(tasking.Record) error {
	return func(r tasking.Record) error {
		var i authority.SignedInstruction
		if err := authority.DecodeStrict(r.Proof, &i); err != nil {
			return err
		}
		if _, err := v.VerifyInstruction(i, "mission"); err != nil {
			return err
		}
		var spec tasking.Spec
		if err := authority.DecodeStrict(i.Payload, &spec); err != nil {
			return err
		}
		if err := verifyControlProof(v, spec, i); err != nil {
			return err
		}
		a, _ := json.Marshal(spec)
		b, _ := json.Marshal(r.Spec)
		if string(a) != string(b) {
			return errors.New("stored mission differs from operator proof")
		}
		return nil
	}
}
func MissionAuthorityContext(v *authority.Verifier) func(context.Context, tasking.Record) error {
	return func(ctx context.Context, r tasking.Record) error {
		var i authority.SignedInstruction
		if err := authority.DecodeStrict(r.Proof, &i); err != nil {
			return err
		}
		if _, err := v.VerifyInstructionContext(ctx, i, "mission"); err != nil {
			return err
		}
		var spec tasking.Spec
		if err := authority.DecodeStrict(i.Payload, &spec); err != nil {
			return err
		}
		if err := verifyControlProof(v, spec, i); err != nil {
			return err
		}
		a, _ := json.Marshal(spec)
		b, _ := json.Marshal(r.Spec)
		if string(a) != string(b) {
			return errors.New("stored mission differs from operator proof")
		}
		return nil
	}
}
func (n *Node) tasksEnabled(w http.ResponseWriter) bool {
	if n.cfg.Tasks == nil {
		http.Error(w, "task service unavailable", http.StatusNotFound)
		return false
	}
	return true
}
func (n *Node) tasks(w http.ResponseWriter, r *http.Request) {
	if !n.tasksEnabled(w) {
		return
	}
	if r.Method == http.MethodGet {
		if _, ok := n.authorize(w, r, "read", nil); !ok {
			return
		}
		records, err := n.cfg.Tasks.ListContext(r.Context())
		if err != nil {
			http.Error(w, "task storage unavailable", 503)
			return
		}
		writeJSON(w, 200, TasksResponse{Tasks: records})
		return
	}
	if !method(w, r, http.MethodPost) {
		return
	}
	i, id, ok := n.instruction(w, r, "mission")
	if !ok {
		return
	}
	record, code, err := n.decideMission(i, id)
	if err != nil && code != http.StatusConflict {
		http.Error(w, err.Error(), code)
		return
	}
	taskResult(w, record, err)
}

// decideMission records authenticated, in-scope refusals for direct and queued
// delivery alike. HTTP failures still report their original admission code.
func (n *Node) decideMission(i authority.SignedInstruction, id authority.Identity) (tasking.Record, int, error) {
	return n.decideMissionContext(context.Background(), i, id)
}
func (n *Node) decideMissionContext(ctx context.Context, i authority.SignedInstruction, id authority.Identity) (tasking.Record, int, error) {
	record, code, err := n.admitMissionContext(ctx, i, id)
	if err == nil {
		return record, code, nil
	}
	var spec tasking.Spec
	if decodeErr := authority.DecodeStrict(i.Payload, &spec); decodeErr != nil || spec.Site != n.cfg.Site || spec.Assignee != n.cfg.TaskAssignee || spec.Domain != mission.Simulation || spec.ID == "" || len(spec.ID) > 256 || spec.Frame.Validate() != nil || spec.NotAfterUnixMS <= i.IssuedAtUnixMS || spec.NotAfterUnixMS > i.ExpiresAtUnixMS || spec.NotAfterUnixMS > id.NotAfter.UnixMilli() {
		return record, code, err
	}
	proof, _ := json.Marshal(i)
	refused, refusalErr := n.cfg.Tasks.Reject(spec, tasking.Authorization{Principal: id.Principal, Operator: true, AllowAutonomy: true, ValidUntil: id.NotAfter, Proof: proof}, "assignee admission refused")
	if refusalErr != nil {
		return tasking.Record{}, http.StatusServiceUnavailable, refusalErr
	}
	return refused, code, err
}

// admitMission is the single assignee admission path for direct and relayed
// operator instructions. Relays never call this on a remote replica of status.
func (n *Node) admitMission(i authority.SignedInstruction, id authority.Identity) (tasking.Record, int, error) {
	return n.admitMissionContext(context.Background(), i, id)
}
func (n *Node) admitMissionContext(ctx context.Context, i authority.SignedInstruction, id authority.Identity) (tasking.Record, int, error) {
	var spec tasking.Spec
	if err := authority.DecodeStrict(i.Payload, &spec); err != nil {
		return tasking.Record{}, 400, errors.New("invalid mission")
	}
	if spec.Type == tasking.InspectType && n.cfg.Evidence == nil {
		return tasking.Record{}, 400, errors.New("inspection requires local evidence store")
	}
	if spec.Trigger != nil && spec.Trigger.Component.Key.Tenant != n.cfg.Tenant {
		return tasking.Record{}, 403, errors.New("trigger outside tenant")
	}
	if !supportedSimulationMission(spec) {
		return tasking.Record{}, 400, errors.New("mission unsupported by current simulation adapter")
	}
	if spec.Mode == mission.Supervised && !n.cfg.SupervisionEnabled {
		return tasking.Record{}, 400, errors.New("authenticated supervision is not configured")
	}
	frameOK := false
	for _, f := range n.cfg.TaskFrames {
		if f == spec.Frame {
			frameOK = true
		}
	}
	if spec.Assignee != n.cfg.TaskAssignee || spec.Site != n.cfg.Site || spec.Domain != mission.Simulation || !frameOK || spec.NotAfterUnixMS > i.ExpiresAtUnixMS || spec.NotAfterUnixMS > id.NotAfter.UnixMilli() {
		return tasking.Record{}, 403, errors.New("mission outside authority or simulation runtime")
	}
	if err := n.validateNativeAdmission(spec, i, false); err != nil {
		return tasking.Record{}, 403, err
	}
	proof, _ := json.Marshal(i)
	// Reserve room for the largest allowed evaluation/inspection body and
	// status fields, so every admitted record fits its replicated attestation.
	base, _ := json.Marshal(tasking.Record{Spec: spec, Proof: proof})
	if len(base) > authority.MaxTaskStatusRecordBytes-MaxBodyBytes-4096 {
		return tasking.Record{}, 400, errors.New("mission record exceeds status replication budget")
	}
	record, err := n.cfg.Tasks.AcceptContext(ctx, spec, tasking.Authorization{Principal: id.Principal, Operator: true, AllowAutonomy: true, ValidUntil: id.NotAfter, Proof: proof, ValidateAdmission: func() error { return n.validateNativeAdmission(spec, i, true) }})
	return record, http.StatusConflict, err
}

// Adapter shape validation does not commission an execution scope. Go2's full
// signed binding must still match the task store's separately configured pins.
// Keep the existing synthetic one-axis carriage restrictions independent.
func supportedSimulationMission(s tasking.Spec) bool {
	if s.InspectionDestination != nil {
		// The explicit destination uses the commissioned planar executor. The
		// normal admission path still verifies the configured context and proof.
		return s.ControlContext != nil && s.Trigger != nil && s.Type == tasking.InspectType && s.Domain == mission.Simulation && s.Mode == mission.Autonomous &&
			tasking.ValidateControlSpec(s, s.ControlContext, s.ControlContext.Context.RunID) == nil
	}
	if s.Type == tasking.Go2GoToType {
		return s.Go2 != nil && tasking.ValidateGo2Spec(s, s.Go2, s.Go2.RunID) == nil
	}
	const epsilon = 1e-9
	return (s.Type == "wendy.nav.go_to.v1" || s.Type == tasking.InspectType) && (s.Mode == mission.Autonomous || s.Mode == mission.Supervised) && s.SpeedMPS > 0 && s.SpeedMPS <= .5 &&
		s.Target[0] >= -2 && s.Target[0] <= 2 && math.Abs(s.Target[1]-.2) <= epsilon && math.Abs(s.Target[2]) <= epsilon
}
func (n *Node) instruction(w http.ResponseWriter, r *http.Request, op string) (authority.SignedInstruction, authority.Identity, bool) {
	body, ok := readBody(w, r)
	if !ok {
		return authority.SignedInstruction{}, authority.Identity{}, false
	}
	transport, ok := n.authorize(w, r, op, body)
	if !ok {
		return authority.SignedInstruction{}, authority.Identity{}, false
	}
	var i authority.SignedInstruction
	err := authority.DecodeStrict(body, &i)
	var id authority.Identity
	if err == nil {
		id, err = n.verifier.VerifyInstructionContext(r.Context(), i, op)
	}
	if err != nil || transport.Principal != id.Principal || transport.GrantID != id.GrantID {
		http.Error(w, "operator instruction rejected", 401)
		return i, id, false
	}
	return i, id, true
}
func (n *Node) taskStop(w http.ResponseWriter, r *http.Request) {
	if !n.tasksEnabled(w) || !method(w, r, http.MethodPost) {
		return
	}
	i, id, ok := n.instruction(w, r, "stop")
	if !ok {
		return
	}
	var req StopRequest
	if err := authority.DecodeStrict(i.Payload, &req); err != nil || req.TaskID == "" {
		http.Error(w, "invalid stop", 400)
		return
	}
	proof, _ := json.Marshal(i)
	record, err := n.cfg.Tasks.StopContext(r.Context(), req.TaskID, tasking.Authorization{Principal: id.Principal, Operator: true, ValidUntil: id.NotAfter, Proof: proof})
	taskResult(w, record, err)
}
func (n *Node) executor(w http.ResponseWriter, r *http.Request) ([]byte, authority.Identity, bool) {
	if !n.tasksEnabled(w) || !method(w, r, http.MethodPost) {
		return nil, authority.Identity{}, false
	}
	b, ok := readBody(w, r)
	if !ok {
		return nil, authority.Identity{}, false
	}
	id, ok := n.authorize(w, r, "execute", b)
	if !ok {
		return nil, id, false
	}
	if id.ExecuteAssignee != n.cfg.TaskAssignee {
		http.Error(w, "executor assignee mismatch", 403)
		return nil, id, false
	}
	return b, id, true
}
func (n *Node) taskExecutorStart(w http.ResponseWriter, r *http.Request) {
	b, id, ok := n.executor(w, r)
	if !ok {
		return
	}
	var req struct{}
	if err := authority.DecodeStrict(b, &req); err != nil {
		http.Error(w, "invalid executor start", 400)
		return
	}
	epoch, err := n.cfg.Tasks.BeginExecutorContext(r.Context(), id.Principal)
	taskResult(w, ExecutorStartResponse{ExecutorEpoch: epoch}, err)
}
func (n *Node) taskEvaluate(w http.ResponseWriter, r *http.Request) {
	if err := r.Context().Err(); err != nil {
		taskResult(w, tasking.Decision{Action: mission.Stop}, err)
		return
	}
	b, id, ok := n.executor(w, r)
	if !ok {
		return
	}
	var req EvaluateRequest
	if err := authority.DecodeStrict(b, &req); err != nil {
		http.Error(w, "invalid evaluation", 400)
		return
	}
	decision, err := n.cfg.Tasks.EvaluateLiveContext(r.Context(), req.TaskID, id.Principal, req.ExecutorEpoch, req.Telemetry)
	taskResult(w, decision, err)
}
func (n *Node) taskReport(w http.ResponseWriter, r *http.Request) {
	b, id, ok := n.executor(w, r)
	if !ok {
		return
	}
	var req ReportRequest
	if err := authority.DecodeStrict(b, &req); err != nil {
		http.Error(w, "invalid report", 400)
		return
	}
	record, err := n.cfg.Tasks.ReportContext(r.Context(), req.TaskID, id.Principal, req.ExecutorEpoch, req.Version, req.Phase)
	taskResult(w, record, err)
}
func taskResult(w http.ResponseWriter, value any, err error) {
	if err != nil {
		http.Error(w, "task operation rejected: "+err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

// InspectionEvidence verifies and pins every result reference before success.
// The task store invokes this under its own lock. Replacing a task's candidate
// hold therefore cannot race a terminal result or accumulate failed captures.
func InspectionEvidence(blobs *evidence.Store) func(tasking.Inspection) error {
	return func(i tasking.Inspection) error {
		if blobs == nil {
			return errors.New("inspection evidence store unavailable")
		}
		if i.TaskID == "" || i.RunID == "" {
			return errors.New("inspection evidence requires task and run identity")
		}
		refs := []evidence.Reference{}
		for _, sample := range i.Samples {
			if ref := sample.Evidence; ref != nil {
				refs = append(refs, evidence.Reference{SHA256: ref.SHA256, Size: ref.Size, ContentType: ref.ContentType})
			}
		}
		_, err := blobs.ReplaceHold(evidence.HoldTask, i.RunID, i.TaskID, refs, 0)
		return err
	}
}

// verifyControlProof validates immutable policy in both admission and ongoing
// authority checks. Proposal expiry is deliberately absent: it is not a lease.
func verifyControlProof(v *authority.Verifier, spec tasking.Spec, i authority.SignedInstruction) error {
	if spec.ControlContext == nil {
		if spec.Proposal != nil {
			return errors.New("proposal requires a control context")
		}
		return nil
	}
	if err := v.VerifyMissionControlContext(*spec.ControlContext); err != nil {
		return err
	}
	if err := tasking.ValidateControlSpec(spec, spec.ControlContext, spec.ControlContext.Context.RunID); err != nil {
		return err
	}
	if err := tasking.ControlProofTime(spec, i.IssuedAtUnixMS, i.ExpiresAtUnixMS); err != nil {
		return err
	}
	return tasking.ValidateControlPayload(i.Payload, spec)
}
func (n *Node) validateNativeAdmission(spec tasking.Spec, i authority.SignedInstruction, firstAcceptance bool) error {
	if err := n.cfg.Tasks.CheckControlContext(spec); err != nil {
		return err
	}
	if err := verifyControlProof(n.verifier, spec, i); err != nil {
		return err
	}
	if spec.ControlContext == nil {
		return nil
	}
	// A retained immutable instruction is a retry, not a new proposal approval.
	// During Store.Accept's fence the task lock is held, so never call Get there.
	if !firstAcceptance {
		if old, err := n.cfg.Tasks.Get(spec.ID); err == nil {
			a, _ := json.Marshal(old.Spec)
			b, _ := json.Marshal(spec)
			proof, _ := json.Marshal(i)
			if string(a) != string(b) || string(old.Proof) != string(proof) {
				return errors.New("task ID already bound to a different instruction")
			}
			return nil
		}
	}
	if spec.Proposal == nil {
		return nil
	}
	ref := spec.Proposal
	if n.cfg.Proposals == nil || ref.Validate() != nil || ref.Audience != n.cfg.Audience {
		return errors.New("proposal unavailable at assignee")
	}
	view, err := n.cfg.Proposals.Get(ref.ID)
	if err != nil {
		return err
	}
	p := view.Record.Proposal
	c := spec.ControlContext.Context
	if view.Expired || view.Record.BodySHA256 != ref.BodySHA256 || p.Tenant != n.cfg.Tenant || p.Site != spec.Site || p.RunID != c.RunID || p.Assignee != spec.Assignee || p.Type != spec.Type || view.Record.Class != spec.Class {
		return errors.New("proposal is expired or outside reviewed mission")
	}
	parameters, err := missioncontrol.DecodeInspectParameters(p.Parameters)
	if err != nil || parameters.Target != spec.Target {
		return errors.New("proposal target differs from reviewed mission")
	}
	if spec.Trigger == nil && parameters.TriggerSHA256 != "" || spec.Trigger != nil && spec.Trigger.SHA256 != parameters.TriggerSHA256 {
		return errors.New("proposal trigger differs from reviewed mission")
	}
	if !missioncontrol.EqualInspectionDestination(parameters.InspectionDestination, spec.InspectionDestination) {
		return errors.New("proposal inspection destination differs from reviewed mission")
	}
	return nil
}
