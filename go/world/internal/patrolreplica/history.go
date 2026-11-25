package patrolreplica

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

var ErrHistoryConflict = errors.New("historical report identity changed; conflicting report held out")

type historicalIdentity struct {
	Digest   string `json:"digest"`
	Conflict bool   `json:"conflict"`
}
type historyState struct {
	Binding string                        `json:"binding"`
	Records map[string]historicalIdentity `json:"records"`
}

// History binds the first verified body to a logical report ID across peer and
// reader restarts. Source-owned register replacement cannot rewrite this history.
type History struct {
	path  string
	lock  *os.File
	state historyState
	fault error
}

func OpenHistory(path string, c Config, reader string) (*History, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("reader history already owned")
	}
	h := &History{path: path, lock: f}
	binding := JSONHash(struct {
		Config Config
		Reader string
	}{c, reader})
	b, e := ReadFile(path, 64<<10)
	if errors.Is(e, os.ErrNotExist) {
		h.state = historyState{binding, map[string]historicalIdentity{}}
		e = AtomicJSON(path, h.state)
	} else if e == nil {
		e = Decode(b, &h.state, 64<<10)
		if h.state.Binding != binding || h.state.Records == nil || len(h.state.Records) > MaxReports {
			e = errors.New("reader history scope or bound changed")
		}
	}
	if e != nil {
		h.Close()
		return nil, e
	}
	return h, nil
}
func (h *History) Close() {
	if h.lock != nil {
		_ = syscall.Flock(int(h.lock.Fd()), syscall.LOCK_UN)
		_ = h.lock.Close()
		h.lock = nil
	}
}
func (h *History) Admit(c Config, reports []Report) ([]Report, error) {
	if h.fault != nil {
		return nil, h.fault
	}
	next := historyState{h.state.Binding, map[string]historicalIdentity{}}
	for id, value := range h.state.Records {
		next.Records[id] = value
	}
	result := []Report{}
	changed, conflict := false, false
	for _, r := range reports {
		if e := r.Validate(c); e != nil {
			return nil, e
		}
		prior, ok := next.Records[r.ID]
		if !ok {
			if len(next.Records) >= MaxReports {
				return nil, errors.New("reader historical identity bound reached")
			}
			prior = historicalIdentity{Digest: r.ContentSHA256}
			next.Records[r.ID] = prior
			changed = true
		} else if prior.Digest != r.ContentSHA256 {
			prior.Conflict = true
			next.Records[r.ID] = prior
			changed = true
		}
		if prior.Conflict {
			conflict = true
			continue
		}
		result = append(result, r)
	}
	if changed {
		if e := AtomicJSON(h.path, next); e != nil {
			h.fault = e
			return nil, e
		}
		h.state = next
	}
	if conflict {
		return result, ErrHistoryConflict
	}
	return result, nil
}
