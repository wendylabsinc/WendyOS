package tasking

import (
	"encoding/json"
	"errors"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
)

// Reject durably binds an authenticated, in-scope instruction to an admission
// refusal. Refusals never enter the execution engine and cannot later resume.
// Authorization must come from the same trusted verifier used by Accept.
func (s *Store) Reject(spec Spec, a Authorization, reason string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	wall, _, err := s.clock()
	if err != nil {
		return Record{}, err
	}
	if spec.ID == "" || len(spec.ID) > 256 || spec.Site != s.cfg.Site || spec.Assignee != s.cfg.Assignee || spec.Domain != s.cfg.Domain || a.Principal == "" || !a.Operator || !json.Valid(a.Proof) || a.ValidUntil.UnixMilli() <= wall || reason == "" || len(reason) > 512 {
		return Record{}, errors.New("invalid verified rejection")
	}
	if r, ok := s.state.Records[spec.ID]; ok {
		return copyRecord(r.Record), nil // the existing assignee decision wins
	}
	if r, ok := s.state.Stops[spec.ID]; ok {
		return copyRecord(r), nil
	}
	d, err := digest(spec)
	if err != nil {
		return Record{}, err
	}
	if r, ok := s.state.Rejected[spec.ID]; ok {
		if r.Digest != d {
			return Record{}, errors.New("rejected task ID bound to different wire spec")
		}
		return copyRecord(r.Record), nil
	}
	if len(s.state.Records)+len(s.state.Stops)+len(s.state.Rejected) >= s.cfg.MaxTasks {
		return Record{}, errors.New("task quota exceeded")
	}
	r := Record{Spec: spec, Status: mission.Status{Version: 1, Phase: mission.Rejected, Reason: reason}, Proof: append(json.RawMessage(nil), a.Proof...), AuthorizationUntilUnixMS: a.ValidUntil.UnixMilli()}
	if s.cfg.ValidateAuthority != nil {
		if err := s.cfg.ValidateAuthority(r); err != nil {
			return Record{}, err
		}
	}
	st := cloneState(s.state)
	if st.Rejected == nil {
		st.Rejected = make(map[string]savedRecord)
	}
	st.Rejected[spec.ID] = savedRecord{Record: copyRecord(r), Digest: d}
	s.appendAudit(&st, spec.ID, a.Principal, "rejected", r.Status, a.Proof)
	if err := s.commit(s.engine.Clone(), st); err != nil {
		return Record{}, err
	}
	return copyRecord(r), nil
}
