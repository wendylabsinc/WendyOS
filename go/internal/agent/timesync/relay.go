package timesync

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
)

// QueryConsensusVia obtains fresh signed authority responses through a bounded
// carrier relay. Nonces and verification stay at this receiver. A relay cannot
// select another authority, assert UTC, or substitute its own server key.
func QueryConsensusVia(ctx context.Context, servers []roughtime.Server, relay roughtime.RelayQuery) (Consensus, error) {
	if relay == nil || len(servers) == 0 || len(servers) > 256 {
		return Consensus{}, errors.New("timesync: invalid authority relay")
	}
	return queryConsensus(ctx, servers, func(ctx context.Context, server roughtime.Server) (roughtime.Result, error) {
		index := -1
		for i, candidate := range servers {
			if candidate.Name == server.Name && candidate.Address == server.Address && bytes.Equal(candidate.PublicKey, server.PublicKey) {
				index = i
				break
			}
		}
		if index < 0 {
			return roughtime.Result{}, errors.New("timesync: unknown relay authority")
		}
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return roughtime.Result{}, err
		}
		raw, err := relay(ctx, uint8(index), nonce[:])
		if err != nil {
			return roughtime.Result{}, err
		}
		if len(raw) == 0 || len(raw) > 4096 {
			return roughtime.Result{}, errors.New("timesync: relay response exceeds bound")
		}
		return roughtime.VerifyResponse(raw, nonce[:], server)
	}, bootTimeNanos)
}

// SyncVia restores a stale device clock through an authenticated neighbour
// without admitting that neighbour's time-window-invalid routing claims. The
// neighbour is only a transport: the existing pinned authority quorum, nonce,
// signature, interval and RTT rules determine whether UTC can advance.
func (m *Manager) SyncVia(ctx context.Context, relay roughtime.RelayQuery) error {
	return m.syncVia(ctx, Servers, relay)
}

func (m *Manager) syncVia(ctx context.Context, servers []roughtime.Server, relay roughtime.RelayQuery) error {
	queryCtx, cancel := context.WithTimeout(ctx, maxCaptureRTT)
	defer cancel()
	c, err := QueryConsensusVia(queryCtx, servers, relay)
	if err != nil {
		return fmt.Errorf("timesync: relayed authenticated query failed: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if !disciplinesClock(c) || c.Quorum < 2 || c.UpperOffsetNanos < c.LowerOffsetNanos {
		return errors.New("timesync: relayed authenticated quorum required")
	}
	boot, err := bootTimeNanos()
	if err != nil {
		return err
	}
	if boot < 0 || c.UpperOffsetNanos > math.MaxInt64-boot {
		return errors.New("timesync: relayed authenticated upper bound overflow")
	}
	lower, upper := time.Unix(0, boot+c.LowerOffsetNanos), time.Unix(0, boot+c.UpperOffsetNanos)
	if lower.Before(floorMin) || upper.After(floorMax) {
		return errors.New("timesync: implausible relayed authenticated interval")
	}
	m.RecordConsensus(c)
	target := lower.Add(upper.Sub(lower) / 2)
	if m.apply != nil {
		m.apply(target)
		return nil
	}
	return m.advance(target)
}
