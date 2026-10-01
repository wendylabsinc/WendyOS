package robotcal

import (
	"encoding/json"
	"time"
)

// Session is a procedure that was interrupted part-way through.
//
// Resumability is not a convenience. A seven-joint sweep on a limp humanoid
// needs two people and a gantry; making them start over because the terminal
// was closed at joint four is how a procedure stops being run at all.
type Session struct {
	ProcedureID string   `json:"procedure_id"`
	Method      MethodID `json:"method"`
	// ProfileKind is what the robot was when the session started. Resuming a
	// sweep against a different profile would silently mix measurements from
	// two joint orders.
	ProfileKind string    `json:"profile_kind"`
	StartedAt   time.Time `json:"started_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Steps holds each finished step's result, keyed by the step id the method
	// chose (a joint name, a sensor pose index). The method owns the encoding;
	// the platform only keeps it and hands it back.
	Steps map[string]json.RawMessage `json:"steps,omitempty"`
	// Skipped names steps the operator chose not to do. It is kept separately
	// from Steps because "did not touch" and "cannot tell" are different
	// answers: a skip that landed in Steps as a zero would qualify.
	Skipped []string `json:"skipped,omitempty"`
}

// Done reports whether a step has already been answered, either by being done
// or by being deliberately skipped.
func (s *Session) Done(step string) bool {
	if _, ok := s.Steps[step]; ok {
		return true
	}
	for _, sk := range s.Skipped {
		if sk == step {
			return true
		}
	}
	return false
}

// WasSkipped reports whether a step was explicitly skipped.
func (s *Session) WasSkipped(step string) bool {
	for _, sk := range s.Skipped {
		if sk == step {
			return true
		}
	}
	return false
}

// Record stores a finished step's result.
func (s *Session) Record(step string, result json.RawMessage) {
	if s.Steps == nil {
		s.Steps = make(map[string]json.RawMessage)
	}
	s.Steps[step] = result
	s.dropSkip(step)
}

// Skip marks a step as deliberately not measured.
func (s *Session) Skip(step string) {
	delete(s.Steps, step)
	if s.WasSkipped(step) {
		return
	}
	s.Skipped = append(s.Skipped, step)
}

func (s *Session) dropSkip(step string) {
	for i, sk := range s.Skipped {
		if sk == step {
			s.Skipped = append(s.Skipped[:i], s.Skipped[i+1:]...)
			return
		}
	}
}

// Progress is how many of the given steps have been answered.
func (s *Session) Progress(steps []string) (done int) {
	for _, step := range steps {
		if s.Done(step) {
			done++
		}
	}
	return done
}
