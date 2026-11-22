package twin

import (
	"errors"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

func (s *Server) resetLocalization(session *robotSession, q twinrobot.Request) error {
	// Serialize with search startup and confirmation. The sequence barriers also
	// keep a matcher that finished before STOP from publishing its old result.
	session.searchMu.Lock()
	defer session.searchMu.Unlock()
	session.pendingMu.Lock()
	defer session.pendingMu.Unlock()
	if q.Sequence <= session.searchRoot || q.Sequence <= session.pendingSequence {
		return errors.New("This localization reset was superseded.")
	}
	if err := s.Store.resetLocalization(q.Frame, session.serial, func() error {
		return session.robot.ResetLocalization(q.Sequence, q.Frame)
	}); err != nil {
		return err
	}
	if session.searchCancel != nil {
		session.searchCancel()
	}
	session.searchCancel = nil
	session.searchRoot = q.Sequence
	session.searchState, session.searchPhase = "", ""
	session.searchMessage = "Position cleared. Find Go2 again."
	session.searchTurns, session.searchMoves = 0, 0
	session.searchReport = nil
	session.pending = twinrobot.SiteFusionReport{}
	session.pendingFrame = ""
	session.pendingSequence = q.Sequence
	session.pendingRoot = 0
	return nil
}

// resetLocalization uses the same Store -> robot lock order as withFrame.
// Room removal cannot race the STOP/reset, and a failed or superseded STOP must
// never erase references. A persistence error is returned so callers cannot
// automatically restart using references that were not actually removed.
func (s *Store) resetLocalization(frame, serial string, reset func() error) error {
	parts := strings.Split(frame, "/")
	if len(parts) != 2 || serial == "" {
		return errors.New("invalid localization reset")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.read(parts[0])
	if err != nil {
		return err
	}
	if r.Removed || !strings.EqualFold(r.Site.ModelRevision, parts[1]) {
		return errors.New("room frame changed")
	}
	if err := reset(); err != nil {
		return err
	}
	if _, exists := r.References[serial]; !exists {
		return nil
	}
	delete(r.References, serial)
	r.Revision++
	return s.write(r)
}
