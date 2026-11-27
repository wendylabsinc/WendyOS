package patrol

import "errors"

// SetSchedule applies the already admitted, signed policy before the first tick.
// Deadlines use executor wall time, independently of sensor simulation timestamps.
// No runtime edit or pause extends the mission or moves an observation deadline.
func (e *Engine) SetSchedule(intervals map[string]int, startedMS int64) error {
	if e.goalNumber != 0 || e.planning != nil || e.State.State != "waiting" || e.scheduled {
		return errors.New("schedule can only be set before patrol starts")
	}
	if len(intervals) == 0 {
		return nil
	}
	if startedMS <= 0 || startedMS >= e.State.ExpiresAtMS || len(intervals) != len(e.State.Rooms) {
		return errors.New("schedule must cover every permitted room before mission expiry")
	}
	for _, r := range e.State.Rooms {
		if n := intervals[r.ID]; n < 10 || n > 86400 {
			return errors.New("revisit intervals must be 10..86400 seconds")
		}
	}
	for i := range e.State.Rooms {
		e.State.Rooms[i].RevisitSeconds = intervals[e.State.Rooms[i].ID]
		e.State.Rooms[i].NextDueMS = startedMS
	}
	e.scheduled = true
	return nil
}
