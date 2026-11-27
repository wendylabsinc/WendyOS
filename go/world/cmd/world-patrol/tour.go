package main

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/patrol"
	"github.com/wendylabsinc/wendy/go/world/internal/patroltour"
)

func tourRooms(rooms []patrol.Room, p *patroltour.Policy) ([]patrol.Room, error) {
	if p == nil {
		return rooms, nil
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	var selected []patrol.Room
	for _, s := range p.Stops {
		found := false
		for _, r := range rooms {
			if r.ID == s.RoomID {
				selected = append(selected, r)
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("tour room is not in the commissioned navigation inventory")
		}
	}
	return selected, nil
}

func (a *adapter) requireInspectionNavigation() error {
	var n patrol.Navigation
	if err := a.request(a.nav, "/status", nil, &n); err != nil {
		return err
	}
	if n.InspectionMode != patroltour.NavigationMode {
		return errors.New("navigation adapter does not support signed fixed-yaw inspection")
	}
	return nil
}
