package main

import (
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolenvironment"
	"github.com/wendylabsinc/wendy/go/world/internal/patroltour"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwatch"
	"github.com/wendylabsinc/wendy/go/world/internal/patrolwork"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"io"
	"os"
)

type scheduleDocument struct {
	Tour           *patroltour.Policy        `json:"tour,omitempty"`
	WorldWatch     *patrolwatch.Policy       `json:"world_watch,omitempty"`
	Environment    *patrolenvironment.Policy `json:"environment,omitempty"`
	Offline        *tasking.PatrolOffline    `json:"-"`
	Work           *patrolwork.Policy        `json:"work,omitempty"`
	Exclusions     *tasking.PatrolExclusions `json:"exclusions,omitempty"`
	Return         *tasking.PatrolReturn     `json:"return,omitempty"`
	RevisitSeconds map[string]int            `json:"revisit_seconds"`
}

func readSchedule(path string, rooms []patrol.Room) (map[string]int, error) {
	cfg, err := readPatrolConfig(path, rooms)
	return cfg.RevisitSeconds, err
}
func readPatrolConfig(path string, rooms []patrol.Room) (scheduleDocument, error) {
	if path == "" {
		return scheduleDocument{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return scheduleDocument{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 {
		return scheduleDocument{}, errors.New("schedule must be a regular JSON file up to 16 KiB")
	}
	d := json.NewDecoder(io.LimitReader(f, 16385))
	d.DisallowUnknownFields()
	var schedule scheduleDocument
	if err = d.Decode(&schedule); err != nil {
		return scheduleDocument{}, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return scheduleDocument{}, errors.New("schedule contains trailing data")
	}
	if schedule.Tour != nil {
		if schedule.Return == nil || len(schedule.RevisitSeconds) != 0 {
			return scheduleDocument{}, errors.New("inspection tour requires a rendezvous and no revisit intervals")
		}
		if err := schedule.Tour.Validate(); err != nil {
			return scheduleDocument{}, err
		}
		if _, err := tourRooms(rooms, schedule.Tour); err != nil {
			return scheduleDocument{}, err
		}
		return schedule, nil
	}
	if len(schedule.RevisitSeconds) != len(rooms) {
		return scheduleDocument{}, errors.New("schedule must contain every permitted room exactly once")
	}
	for _, room := range rooms {
		if n := schedule.RevisitSeconds[room.ID]; n < 10 || n > 86400 {
			return scheduleDocument{}, errors.New("each permitted room requires a revisit interval of 10..86400 seconds")
		}
	}
	return schedule, nil
}
