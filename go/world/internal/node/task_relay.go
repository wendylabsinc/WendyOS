package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/diagnostics"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/taskrelay"
)

type TaskRelayResponse struct {
	Entries   []taskrelay.Entry `json:"entries"`
	LastKnown bool              `json:"last_known"`
}

func (n *Node) taskRelay(w http.ResponseWriter, r *http.Request) {
	if n.cfg.TaskRelay == nil {
		http.Error(w, "task relay unavailable", http.StatusNotFound)
		return
	}
	if r.Method == http.MethodGet {
		if _, ok := n.authorize(w, r, "read", nil); !ok {
			return
		}
		entries, err := n.cfg.TaskRelay.List()
		if err != nil {
			http.Error(w, "task relay storage unavailable", 503)
			return
		}
		// Retain expired delivery history, but do not present a revoked or
		// currently unverifiable gateway's status as authoritative.
		for j := range entries {
			if entries[j].Status != nil {
				if _, err := n.verifier.VerifyTaskStatusContext(r.Context(), *entries[j].Status); err != nil {
					entries[j].Status = nil
				}
			}
		}
		writeJSON(w, 200, TaskRelayResponse{Entries: entries, LastKnown: true})
		return
	}
	if !method(w, r, http.MethodPost) {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var instruction authority.SignedInstruction
	if err := authority.DecodeStrict(body, &instruction); err != nil || (instruction.Operation != "mission" && instruction.Operation != "stop") {
		http.Error(w, "invalid routed operator instruction", 400)
		return
	}
	transport, ok := n.authorize(w, r, instruction.Operation, body)
	if !ok {
		return
	}
	if _, ok := n.cfg.TaskRoutes[instruction.Audience]; !ok {
		http.Error(w, "unknown final task destination", 403)
		return
	}
	operator, err := n.verifier.VerifyInstructionForAudienceContext(r.Context(), instruction, instruction.Operation, instruction.Audience)
	if err != nil || transport.Principal != operator.Principal || transport.GrantID != operator.GrantID {
		http.Error(w, "original operator instruction rejected", 401)
		return
	}
	entry, err := n.cfg.TaskRelay.PutInstructionContext(r.Context(), instruction)
	if err != nil {
		http.Error(w, "task relay refused instruction: "+err.Error(), 409)
		return
	}
	// This acknowledges durable ingress, not admission or physical execution.
	writeJSON(w, http.StatusAccepted, struct {
		State string          `json:"state"`
		Entry taskrelay.Entry `json:"entry"`
	}{State: "queued", Entry: entry})
}

func (n *Node) taskRelayReplica(w http.ResponseWriter, r *http.Request) {
	if n.cfg.TaskRelay == nil {
		http.Error(w, "task relay unavailable", 404)
		return
	}
	if !method(w, r, http.MethodGet) {
		return
	}
	if _, ok := n.authorize(w, r, "replicate", nil); !ok {
		return
	}
	entries, err := n.cfg.TaskRelay.ExportContext(r.Context())
	if err != nil {
		http.Error(w, "task relay export unavailable", 503)
		return
	}
	writeJSON(w, 200, TaskRelayResponse{Entries: entries, LastKnown: true})
}

// ProcessTaskRelay delivers only instructions addressed to this assignee. Its
// read-only replicas of other assignees' states can never enter the task engine.
func (n *Node) ProcessTaskRelay() error {
	return n.ProcessTaskRelayContext(context.Background())
}
func (n *Node) ProcessTaskRelayContext(ctx context.Context) error {
	if !diagnostics.Enabled(ctx) {
		var request *diagnostics.Request
		ctx, request = n.cfg.Trace.Start(ctx, diagnostics.TaskRelayRound)
		defer request.Finish(0)
	}
	if n.cfg.TaskRelay == nil || n.cfg.Tasks == nil {
		return nil
	}
	n.relayMu.Lock()
	defer n.relayMu.Unlock()
	pending, err := n.cfg.TaskRelay.PendingContext(ctx, n.cfg.Audience)
	if err != nil {
		return err
	}
	var failures []error
	for _, instruction := range pending {
		operator, err := n.verifier.VerifyInstructionContext(ctx, instruction, instruction.Operation)
		if err != nil {
			continue
		} // authority can expire between selection and dispatch
		proof, _ := json.Marshal(instruction)
		a := tasking.Authorization{Principal: operator.Principal, Operator: true, AllowAutonomy: true, ValidUntil: operator.NotAfter, Proof: proof}
		if instruction.Operation == "stop" {
			var stop StopRequest
			if err = authority.DecodeStrict(instruction.Payload, &stop); err == nil {
				_, err = n.cfg.Tasks.StopContext(ctx, stop.TaskID, a)
			}
		} else {
			var record tasking.Record
			record, _, err = n.decideMissionContext(ctx, instruction, operator)
			if record.Spec.ID != "" {
				err = nil
			} // an existing or refused assignee decision is definitive
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	// A crash between task-store commit and status publication is repaired here.
	// Reopening the task store first fails interrupted motion before we sign it.
	if err := n.refreshTaskStatusesContext(ctx); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (n *Node) refreshTaskStatuses() error {
	return n.refreshTaskStatusesContext(context.Background())
}
func (n *Node) refreshTaskStatusesContext(ctx context.Context) error {
	local, err := n.cfg.Tasks.ListContext(ctx)
	if err != nil {
		return err
	}
	entries, err := n.cfg.TaskRelay.List()
	if err != nil {
		return err
	}
	prior := make(map[string]*authority.SignedTaskStatus)
	for _, entry := range entries {
		if entry.Audience == n.cfg.Audience {
			prior[entry.TaskID] = entry.Status
		}
	}
	var failures []error
	for _, record := range local {
		raw, err := json.Marshal(record)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if old := prior[record.Spec.ID]; old != nil && bytes.Equal(old.Record, raw) {
			oldGrant, _ := json.Marshal(old.Grant)
			currentGrant, _ := json.Marshal(n.cfg.TaskStatusCredentials.Grant)
			if bytes.Equal(oldGrant, currentGrant) {
				if _, err := n.verifier.VerifyTaskStatusContext(ctx, *old); err == nil {
					continue
				}
			}
		}
		credentials := n.cfg.TaskStatusCredentials
		status, err := authority.SignTaskStatus(credentials.Grant, credentials.PrivateKey, n.cfg.Audience, n.cfg.TaskAssignee, record.Spec.ID, record.Status.Version, record.Status.Phase.Terminal(), raw, time.Now())
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if _, err = n.cfg.TaskRelay.PutStatusContext(ctx, status); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (n *Node) syncTaskRelay(ctx context.Context, peer Peer, credentials Credentials) error {
	localErr := n.ProcessTaskRelayContext(ctx)
	endpoint, err := url.Parse(peer.URL)
	if err != nil || endpoint.Scheme != "http" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.Join(localErr, errors.New("invalid simulation peer URL"))
	}
	endpoint.Path = "/v1/task-relay/replica"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return errors.Join(localErr, err)
	}
	if err = AuthorizeRequest(req, credentials, peer.Audience, "replicate", nil); err != nil {
		return errors.Join(localErr, err)
	}
	response, err := n.client.Do(req)
	if err != nil {
		return errors.Join(localErr, err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return errors.Join(localErr, fmt.Errorf("task relay peer returned %d", response.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil || len(body) > MaxResponseBytes {
		return errors.Join(localErr, errors.New("invalid or oversized task relay response"))
	}
	var reply TaskRelayResponse
	if err = authority.DecodeStrictLimit(body, &reply, MaxResponseBytes); err != nil {
		return errors.Join(localErr, err)
	}
	if reply.Entries == nil || len(reply.Entries) > 10000 || !reply.LastKnown {
		return errors.Join(localErr, errors.New("invalid task relay response"))
	}
	failures := []error{localErr}
	// A bad or expired item must not starve independent stops and task states.
	for _, entry := range reply.Entries {
		if entry.Status != nil {
			if _, err := n.cfg.TaskRelay.PutStatus(*entry.Status); err != nil {
				failures = append(failures, err)
			}
		}
	}
	for _, entry := range reply.Entries {
		if entry.Stop != nil {
			if _, err := n.cfg.TaskRelay.PutInstruction(*entry.Stop); err != nil {
				failures = append(failures, err)
			}
		}
	}
	for _, entry := range reply.Entries {
		if entry.Mission != nil {
			if _, err := n.cfg.TaskRelay.PutInstruction(*entry.Mission); err != nil {
				failures = append(failures, err)
			}
		}
	}
	if err := n.ProcessTaskRelay(); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
