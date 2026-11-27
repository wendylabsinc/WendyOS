package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/taskhistory"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"io"
	"net/http"
	"strconv"
	"time"
)

func (n *Node) configureTaskHistory() error {
	if n.cfg.TaskHistory == nil {
		if n.cfg.ExportTaskHistory {
			return errors.New("history exporter requires history storage")
		}
		return nil
	}
	if !n.cfg.TaskHistory.MatchesScope(n.cfg.Tenant, n.cfg.Site) {
		return errors.New("history scope mismatch")
	}
	if n.cfg.ExportTaskHistory {
		g := n.cfg.TaskStatusCredentials.Grant.Grant
		o, ok := n.cfg.TaskHistory.Origins()[n.cfg.Audience]
		if n.cfg.Tasks == nil || !ok || g.Principal != o.Principal || g.TaskStatusAudience != n.cfg.Audience || g.TaskStatusAssignee != n.cfg.TaskAssignee || o.Assignee != n.cfg.TaskAssignee {
			return errors.New("history exporter must be the configured assignee")
		}
		page, e := n.cfg.Tasks.ReadAudit(0, "", "", 1)
		if e != nil || page.RunID != o.RunID {
			return errors.New("history run mismatch")
		}
		if _, e := n.verifier.VerifyGrant(n.cfg.TaskStatusCredentials.Grant, "task-status"); e != nil {
			return e
		}
		if len(n.cfg.TaskStatusCredentials.PrivateKey) != 64 || !bytes.Equal(n.cfg.TaskStatusCredentials.PrivateKey[32:], g.PublicKey) {
			return errors.New("history signer mismatch")
		}
	}
	return nil
}
func (n *Node) exportTaskHistory(ctx context.Context, q taskhistory.Request) (taskhistory.Page, error) {
	if n.cfg.TaskHistory == nil {
		return taskhistory.Page{}, errors.New("history unavailable")
	}
	if !n.cfg.ExportTaskHistory || q.Origin != n.cfg.Audience {
		return n.cfg.TaskHistory.Export(ctx, q)
	}
	o, ok := n.cfg.TaskHistory.Origins()[q.Origin]
	if !ok || o.RunID != q.RunID {
		return taskhistory.Page{}, taskhistory.ErrInvalid
	}
	if _, e := n.verifier.VerifyGrantContext(ctx, n.cfg.TaskStatusCredentials.Grant, "task-status"); e != nil {
		return taskhistory.Page{}, e
	}
	page, e := n.cfg.Tasks.ReadAudit(q.After, q.Incarnation, q.PreviousSHA256, 16)
	if e != nil {
		return taskhistory.Page{}, e
	}
	out := taskhistory.Page{Origin: q.Origin, RunID: page.RunID, Incarnation: page.Incarnation, After: q.After, Through: q.After, PreviousSHA256: page.PreviousSHA256, Records: []taskhistory.Receipt{}, More: page.More}
	previous := page.PreviousSHA256
	for _, a := range page.Events {
		raw, _ := json.Marshal(a)
		r, e := authority.SignTaskAudit(authority.SignedTaskAudit{Grant: n.cfg.TaskStatusCredentials.Grant, Audience: q.Origin, Assignee: o.Assignee, RunID: o.RunID, Incarnation: page.Incarnation, Sequence: a.Sequence, PreviousSHA256: previous, Event: raw, ExportedUnixMS: time.Now().UnixMilli()}, n.cfg.TaskStatusCredentials.PrivateKey)
		if e != nil {
			return out, e
		}
		candidate := out
		candidate.Records = append(append([]taskhistory.Receipt{}, out.Records...), r)
		candidate.Through = a.Sequence
		b, _ := json.Marshal(candidate)
		if len(b)+1 > taskhistory.MaxPageBytes {
			out.More = true
			break
		}
		out = candidate
		previous = tasking.AuditEventHash(previous, a)
	}
	if out.More && len(out.Records) == 0 {
		return out, taskhistory.ErrCapacity
	}
	if _, e := n.verifier.VerifyGrantContext(ctx, n.cfg.TaskStatusCredentials.Grant, "task-status"); e != nil {
		return taskhistory.Page{}, e
	}
	return out, nil
}
func (n *Node) taskHistoryReplica(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if _, ok = n.authorize(w, r, "replicate", body); !ok {
		return
	}
	var q taskhistory.Request
	if authority.DecodeStrict(body, &q) != nil {
		http.Error(w, "invalid history request", 400)
		return
	}
	page, e := n.exportTaskHistory(r.Context(), q)
	if e != nil {
		http.Error(w, "history export unavailable", 409)
		return
	}
	writeJSON(w, 200, page)
}
func (n *Node) refreshLocalHistory(ctx context.Context) error {
	if !n.cfg.ExportTaskHistory {
		return nil
	}
	for i := 0; i < 8; i++ {
		q, e := n.cfg.TaskHistory.Cursor(n.cfg.Audience)
		if e != nil {
			return e
		}
		p, e := n.exportTaskHistory(ctx, q)
		if e != nil {
			return e
		}
		if e = n.cfg.TaskHistory.Import(ctx, p, n.cfg.Audience); e != nil {
			return e
		}
		if !p.More {
			return nil
		}
	}
	return nil
}
func (n *Node) taskHistoryRead(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if _, ok := n.authorize(w, r, "read", nil); !ok {
		return
	}
	if n.cfg.TaskHistory == nil {
		http.Error(w, "history unavailable", 404)
		return
	}
	q := r.URL.Query()
	for k := range q {
		if k != "origin" && k != "task_id" && k != "limit" && k != "after" {
			http.Error(w, "unknown history parameter", 400)
			return
		}
		if len(q[k]) != 1 {
			http.Error(w, "duplicate history parameter", 400)
			return
		}
	}
	limit := 20
	if q.Get("limit") != "" {
		var e error
		limit, e = strconv.Atoi(q.Get("limit"))
		if e != nil {
			http.Error(w, "invalid limit", 400)
			return
		}
	}
	if q.Get("origin") == n.cfg.Audience {
		_ = n.refreshLocalHistory(r.Context())
	}
	view, e := n.cfg.TaskHistory.List(r.Context(), q.Get("origin"), q.Get("task_id"), limit, q.Get("after"))
	if e != nil {
		http.Error(w, "history read unavailable or invalid cursor", 409)
		return
	}
	writeJSON(w, 200, view)
}
func (n *Node) syncTaskHistory(ctx context.Context, peer Peer, credentials Credentials) error {
	if n.cfg.TaskHistory == nil {
		return nil
	}
	var failures []error
	for origin := range n.cfg.TaskHistory.Origins() {
		if n.cfg.ExportTaskHistory && origin == n.cfg.Audience {
			if e := n.refreshLocalHistory(ctx); e != nil {
				failures = append(failures, e)
			}
			continue
		}
		for i := 0; i < 8; i++ {
			q, e := n.cfg.TaskHistory.Cursor(origin)
			if e != nil {
				return e
			}
			body, _ := json.Marshal(q)
			req, e := http.NewRequestWithContext(ctx, http.MethodPost, peer.URL+"/v1/task-history/replica", bytes.NewReader(body))
			if e != nil {
				return e
			}
			req.Header.Set("Content-Type", "application/json")
			if e = AuthorizeRequest(req, credentials, peer.Audience, "replicate", body); e != nil {
				return e
			}
			res, e := n.client.Do(req)
			if e != nil {
				failures = append(failures, e)
				break
			}
			b, e := io.ReadAll(io.LimitReader(res.Body, taskhistory.MaxPageBytes+1))
			res.Body.Close()
			var p taskhistory.Page
			if e != nil || res.StatusCode != 200 || len(b) > taskhistory.MaxPageBytes || authority.DecodeStrictLimit(b, &p, taskhistory.MaxPageBytes) != nil || p.Origin != q.Origin || p.RunID != q.RunID || p.After != q.After || p.PreviousSHA256 != q.PreviousSHA256 || (q.Incarnation != "" && p.Incarnation != q.Incarnation) {
				failures = append(failures, fmt.Errorf("peer history unavailable or scope mismatch"))
				break
			}
			if e = n.cfg.TaskHistory.Import(ctx, p, peer.Audience); e != nil {
				failures = append(failures, e)
				break
			}
			if !p.More {
				break
			}
		}
	}
	return errors.Join(failures...)
}
