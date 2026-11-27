package tasking

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// AuditPage is an exact ordered slice of this assignee's durable journal. It
// does not dispatch work. Hashes bind order and original serialized event data.
type AuditPage struct {
	RunID          string
	Incarnation    string
	PreviousSHA256 string
	Events         []Audit
	More           bool
}

func AuditEventHash(previous string, event Audit) string {
	b, _ := json.Marshal(event)
	h := sha256.Sum256(append([]byte(previous+"\n"), b...))
	return hex.EncodeToString(h[:])
}
func (s *Store) ReadAudit(after uint64, incarnation string, previous string, limit int) (AuditPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.ready(); e != nil {
		return AuditPage{}, e
	}
	if limit < 1 || limit > 16 || after > uint64(len(s.state.Audit)) || (incarnation != "" && incarnation != s.state.AuditID) || (after > 0 && incarnation == "") {
		return AuditPage{}, errors.New("invalid audit cursor")
	}
	hash := strings.Repeat("0", 64)
	for i := uint64(0); i < after; i++ {
		hash = AuditEventHash(hash, s.state.Audit[i])
	}
	if previous != "" && previous != hash {
		return AuditPage{}, errors.New("audit prefix mismatch")
	}
	out := AuditPage{RunID: s.cfg.RunID, Incarnation: s.state.AuditID, PreviousSHA256: hash, Events: []Audit{}}
	bytes := 0
	for i := after; i < uint64(len(s.state.Audit)) && len(out.Events) < limit; i++ {
		event := s.state.Audit[i]
		b, _ := json.Marshal(event)
		if len(b) > 512<<10 {
			return AuditPage{}, errors.New("audit event exceeds export bound")
		}
		if bytes+len(b) > 700<<10 {
			break
		}
		bytes += len(b)
		event.Proof = append(json.RawMessage(nil), event.Proof...)
		out.Events = append(out.Events, event)
	}
	out.More = after+uint64(len(out.Events)) < uint64(len(s.state.Audit))
	return out, nil
}
