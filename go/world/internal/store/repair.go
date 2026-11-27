package store

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/subscription"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const MaxRepairRecords = 8
const maxRepairComponentBytes = 512 << 10

type Publication struct {
	Source    world.Source
	Component world.Component
	// ExpiresAt is an optional absolute transfer cap, never an extension.
	ExpiresAt time.Time
}

// BatchPublicationState contains isolated copies of all retained registers and
// locally allocated epochs, including expired entries and tombstones.
type BatchPublicationState struct {
	Records []world.Component
	Epochs  map[world.Source]uint64
}

// ApplyPreparedBatch atomically commits a bounded page. prepare must persist
// proof coverage for both the old world and every candidate before returning
// nil. It runs under the store lock and must not reenter Store, Node or authority.
// No page result is exposed until the world write succeeds. Known denials retain
// the staged observation floor; writer errors poison the store without retry.
// Structural page bounds are checked before observing the transaction clock.
func (s *Store) ApplyPreparedBatch(page []Publication, prepare func(BatchPublicationState) error) ([]world.Result, error) {
	if len(page) == 0 || len(page) > MaxRepairRecords || prepare == nil {
		return nil, errors.New("invalid repair page or preparation callback")
	}
	components := make([]world.Component, len(page))
	sources := make([]world.Source, len(page))
	caps := make([]time.Time, len(page))
	for i, p := range page {
		if err := p.Source.Validate(); err != nil {
			return nil, err
		}
		sources[i], components[i] = p.Source, p.Component
		caps[i] = p.ExpiresAt
	}
	encoded, err := json.Marshal(components)
	if err != nil {
		return nil, err
	}
	encodedCaps, err := json.Marshal(caps)
	if err != nil {
		return nil, err
	}
	if len(encoded)+len(encodedCaps) > maxRepairComponentBytes {
		return nil, errors.New("repair components exceed byte limit")
	}
	var copied []world.Component
	if err := json.Unmarshal(encoded, &copied); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	observation, err := s.candidate()
	if err != nil {
		return nil, err
	}
	records, err := observation.Records()
	if err != nil {
		return nil, err
	}
	staged, err := s.prepare(observation, s.epochs)
	if err != nil {
		return nil, err
	}
	deny := func(reason error) ([]world.Result, error) {
		if err := s.commitPrepared(staged); err != nil {
			return nil, err
		}
		return nil, reason
	}
	candidate, err := observation.Clone()
	if err != nil {
		return deny(err)
	}
	epochs := make(map[world.Source]uint64, len(s.epochs))
	for source, epoch := range s.epochs {
		epochs[source] = epoch
	}
	for _, c := range copied {
		if !s.subscription.Allows(c.Key) {
			return deny(subscription.ErrExcluded)
		}
	}
	if err := prepare(BatchPublicationState{Records: records, Epochs: epochs}); err != nil {
		return deny(err)
	}
	results := make([]world.Result, len(copied))
	for i, c := range copied {
		results[i], err = candidate.Apply(sources[i], c)
		if err != nil {
			return deny(err)
		}
		if !caps[i].IsZero() {
			if err := candidate.ClampExpiry(c.Key, c.Version, caps[i]); err != nil {
				return deny(err)
			}
		}
		if !s.subscription.Retains(c) {
			if e := candidate.Fence(c.Key, c.Version); e != nil {
				return deny(e)
			}
		}
	}
	publication, err := s.prepare(candidate, s.epochs)
	if err != nil {
		return deny(err)
	}
	if err := s.commitPrepared(publication); err != nil {
		return nil, err
	}
	return results, nil
}

// RepairSnapshot durably advances the observed clock and returns the exact
// committed retained entries and expiry caps. The copied checkpoint is trusted
// local state for constructing transfer inventories, never network authority.
func (s *Store) RepairSnapshot() (world.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.candidate()
	if err != nil {
		return world.Checkpoint{}, err
	}
	prepared, err := s.prepare(r, s.epochs)
	if err != nil {
		return world.Checkpoint{}, err
	}
	if err := s.commitPrepared(prepared); err != nil {
		return world.Checkpoint{}, err
	}
	return prepared.checkpoint, nil
}
