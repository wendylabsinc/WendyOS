package authority

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"time"
)

const MaxTaskStatusRecordBytes = 512 << 10
const MaxTaskStatusBytes = 1 << 20

// SignedTaskStatus is a gateway's read-only attestation to one durable assignee
// record. Record must be decoded and bound to the duplicate task/assignee/status
// fields by the task replica before storage. It never authorizes actuation.
// SignedAtUnixMS is the origin's observation time, not a relay receipt time.
// History has no expiry; consumers must show nonterminal state as last-known.
type SignedTaskStatus struct {
	Version        int             `json:"version"`
	Grant          SignedGrant     `json:"grant"`
	Audience       string          `json:"audience"`
	Assignee       string          `json:"assignee"`
	TaskID         string          `json:"task_id"`
	StatusVersion  uint64          `json:"status_version"`
	Terminal       bool            `json:"terminal"`
	Record         json.RawMessage `json:"record"`
	SignedAtUnixMS int64           `json:"signed_at_unix_ms"`
	Signature      []byte          `json:"signature"`
}

func taskStatusPayload(s SignedTaskStatus) any {
	g, _ := json.Marshal(s.Grant)
	return struct {
		Version        int             `json:"version"`
		GrantDigest    string          `json:"grant_digest"`
		Audience       string          `json:"audience"`
		Assignee       string          `json:"assignee"`
		TaskID         string          `json:"task_id"`
		StatusVersion  uint64          `json:"status_version"`
		Terminal       bool            `json:"terminal"`
		Record         json.RawMessage `json:"record"`
		SignedAtUnixMS int64           `json:"signed_at_unix_ms"`
	}{s.Version, hash(g), s.Audience, s.Assignee, s.TaskID, s.StatusVersion, s.Terminal, s.Record, s.SignedAtUnixMS}
}

func validTaskStatus(s SignedTaskStatus) bool {
	g := s.Grant.Grant
	if s.Version != Version || s.Audience == "" || len(s.Audience) > 256 || s.Assignee == "" || len(s.Assignee) > 256 || s.TaskID == "" || len(s.TaskID) > 256 || s.StatusVersion == 0 || s.SignedAtUnixMS <= 0 || g.Kind != "device" || !contains(g.Scopes, "task-status") || s.Audience != g.TaskStatusAudience || s.Assignee != g.TaskStatusAssignee || len(s.Record) == 0 || len(s.Record) > MaxTaskStatusRecordBytes || !json.Valid(s.Record) {
		return false
	}
	b, err := json.Marshal(s)
	return err == nil && len(b) <= MaxTaskStatusBytes
}

func SignTaskStatus(g SignedGrant, key ed25519.PrivateKey, audience, assignee, taskID string, statusVersion uint64, terminal bool, record json.RawMessage, observedAt time.Time) (SignedTaskStatus, error) {
	s := SignedTaskStatus{Version: Version, Grant: g, Audience: audience, Assignee: assignee, TaskID: taskID, StatusVersion: statusVersion, Terminal: terminal, Record: append(json.RawMessage(nil), record...), SignedAtUnixMS: observedAt.UnixMilli()}
	if !privateMatches(key, g.Grant.PublicKey) || validateGrant(g.Grant, 365*24*time.Hour) != nil || !validTaskStatus(s) || s.SignedAtUnixMS < g.Grant.NotBeforeUnixMS || s.SignedAtUnixMS >= g.Grant.NotAfterUnixMS {
		return SignedTaskStatus{}, ErrDenied
	}
	var err error
	s.Signature, err = sign(key, "task-status", taskStatusPayload(s))
	if err != nil || !validTaskStatus(s) {
		return SignedTaskStatus{}, ErrDenied
	}
	return s, nil
}

// VerifyTaskStatus authenticates the assignee gateway at its signed observation
// time and applies current gateway revocation. Expiry of its grant does not
// erase recorded outcomes. Future observations and stale revocation state fail.
// No operator proof or task semantic validation is implied by this method.
func (v *Verifier) VerifyTaskStatus(s SignedTaskStatus) (Identity, error) {
	return v.VerifyTaskStatusContext(context.Background(), s)
}
func (v *Verifier) VerifyTaskStatusContext(ctx context.Context, s SignedTaskStatus) (Identity, error) {
	hold := v.lockContext(ctx)
	defer v.mu.Unlock()
	defer hold.End()
	return v.verifyTaskStatusContext(ctx, s, false)
}

// VerifyTaskStatusHistory verifies retained storage's origin signature and the
// gateway grant at observation time, without applying current revocations. It
// is only for reopening previously verified local state. Import, export and
// user-visible trusted reads must call VerifyTaskStatus instead, so revocation
// of a gateway cannot be bypassed by presenting a previously stored record.
func (v *Verifier) VerifyTaskStatusHistory(s SignedTaskStatus) (Identity, error) {
	v.lock()
	defer v.mu.Unlock()
	return v.verifyTaskStatus(s, true)
}

func (v *Verifier) verifyTaskStatus(s SignedTaskStatus, history bool) (Identity, error) {
	return v.verifyTaskStatusContext(context.Background(), s, history)
}
func (v *Verifier) verifyTaskStatusContext(ctx context.Context, s SignedTaskStatus, history bool) (Identity, error) {
	now := v.cfg.Now()
	if !validTaskStatus(s) || s.SignedAtUnixMS > now.UnixMilli() || !verify(s.Grant.Grant.PublicKey, "task-status", taskStatusPayload(s), s.Signature) {
		return Identity{}, ErrDenied
	}
	if history {
		g := s.Grant.Grant
		if err := validateGrant(g, v.cfg.MaxGrantLifetime); err != nil {
			return Identity{}, err
		}
		if !verify(v.cfg.RootPublicKey, "grant", g, s.Grant.Signature) || g.Tenant != v.cfg.Tenant || g.Site != v.cfg.Site || g.Domain != v.cfg.Domain || s.SignedAtUnixMS < g.NotBeforeUnixMS || s.SignedAtUnixMS >= g.NotAfterUnixMS {
			return Identity{}, ErrDenied
		}
		return grantIdentity(g), nil
	}
	return v.identityContext(ctx, s.Grant, "task-status", time.UnixMilli(s.SignedAtUnixMS), now)
}
