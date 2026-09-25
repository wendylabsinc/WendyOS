package models

import "time"

// Clock is the time source the supervisor schedules with; tests use a fake.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a scheduled callback.
type Timer interface{ Stop() bool }

type realClock struct{}

func (realClock) Now() time.Time                            { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
