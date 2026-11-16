package meshsession

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
)

// Only the logical coordinator proposes a new epoch. The participant answers
// BUSY immediately if it cannot reserve admission; it never waits for existing
// application dependencies while the coordinator holds a reservation.
// Five bytes per record: kind and next carrier-local big-endian alias.
const (
	carrierPropose byte = 22 + iota
	carrierAccept
	carrierBusy
	carrierUnavailable
)

func (c *directCarrierLane) proposeReuse(ctx context.Context, role byte, retired <-chan struct{}, permitted func() bool, reserve func() (func(), bool)) (release func(), err error) {
	return c.proposeReuseWithin(ctx, role, retired, permitted, reserve, time.Minute)
}

// A paused peer may leave routing alive without completing its proposal. Do
// not hold new application admission indefinitely; the carrier owner retries
// the physical connection after this bounded setup failure.
func (c *directCarrierLane) proposeReuseWithin(ctx context.Context, role byte, retired <-chan struct{}, permitted func() bool, reserve func() (func(), bool), timeout time.Duration) (release func(), err error) {
	if c.alias == math.MaxUint32 || c.pending != nil || role > 1 || permitted == nil || reserve == nil {
		return nil, directstream.ErrBounds
	}
	readCtx, cancel := context.WithCancel(ctx)
	result := make(chan directRead, 1)
	reading := false
	startRead := func() {
		if reading || c.pending != nil {
			return
		}
		reading = true
		go func() {
			wire, err := c.DirectRecordCarrier.ReceiveDirectRecord(readCtx)
			result <- directRead{data: wire, err: err}
		}()
	}
	var held func()
	var heldUntil time.Time
	localRetired, accepted, proposed := false, false, false
	var outgoing byte
	retry := time.Time{}
	retryDelay := 250 * time.Millisecond
	consume := func(read directRead) error {
		if read.err != nil {
			return read.err
		}
		wire := read.data
		if len(wire) < 5 || len(wire) > directstream.MaxFlowFrame {
			return directstream.ErrBounds
		}
		alias := binary.BigEndian.Uint32(wire[1:5])
		if alias == 0 {
			return directstream.ErrBounds
		}
		if alias <= c.alias {
			return nil
		}
		if alias != c.alias+1 {
			return directstream.ErrBounds
		}
		if len(wire) == 5 {
			switch wire[0] {
			case carrierUnavailable:
				return localmesh.ErrDirectLaneRetired
			case carrierPropose:
				if role != 1 || held != nil || outgoing != 0 || accepted {
					return directstream.ErrBounds
				}
				outgoing = carrierBusy
				if localRetired && permitted() {
					if release, ok := reserve(); ok {
						held = release
						heldUntil = time.Now().Add(timeout)
						outgoing = carrierAccept
					}
				}
				return nil
			case carrierAccept:
				if role != 0 || !proposed || held == nil {
					return directstream.ErrBounds
				}
				accepted = true
				return nil
			case carrierBusy:
				if role != 0 || !proposed || held == nil {
					return directstream.ErrBounds
				}
				held()
				held = nil
				proposed = false
				retry = time.Now().Add(retryDelay)
				retryDelay = min(2*retryDelay, 4*time.Second)
				return nil
			}
		}
		offer, e := directstream.DecodeControlRecord(wire)
		if role != 1 || !accepted || e != nil || offer.Kind != directstream.ControlOffer || !offer.Create || offer.Mode != directstream.DirectBLE || offer.Count != 0 {
			return directstream.ErrBounds
		}
		c.pending = append([]byte(nil), wire...)
		return nil
	}
	defer func() {
		cancel()
		if reading {
			<-result
		}
		if err != nil && held != nil {
			held()
		}
	}()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if held != nil && !time.Now().Before(heldUntil) {
			return nil, fmt.Errorf("BLE reuse admission: %w", context.DeadlineExceeded)
		}
		if localRetired && !permitted() {
			if held != nil {
				held()
				held = nil
			}
			outgoing = carrierUnavailable
		}
		if role == 0 && localRetired && permitted() && held == nil && outgoing == 0 && !time.Now().Before(retry) {
			if release, ok := reserve(); ok {
				held = release
				heldUntil = time.Now().Add(timeout)
				outgoing = carrierPropose
			}
		}
		if outgoing != 0 {
			wire := [5]byte{outgoing}
			binary.BigEndian.PutUint32(wire[1:], c.alias+1)
			sent, e := c.TrySendDirectRecord(wire[:])
			if e != nil {
				return nil, e
			}
			if sent {
				switch outgoing {
				case carrierPropose:
					proposed = true
				case carrierAccept:
					accepted = true
				case carrierUnavailable:
					return nil, localmesh.ErrDirectLaneRetired
				}
				outgoing = 0
			}
		}
		if accepted {
			cancel()
			if reading {
				read := <-result
				reading = false
				if read.err != nil && !errors.Is(read.err, context.Canceled) {
					return nil, read.err
				}
				if read.err == nil {
					if e := consume(read); e != nil {
						return nil, e
					}
				}
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return held, nil
		}
		startRead()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-retired:
			localRetired = true
			retired = nil
		case read := <-result:
			reading = false
			if e := consume(read); e != nil {
				return nil, e
			}
		case <-ticker.C:
		}
	}
}

// New callers wait only until the agreed fresh epoch is established. Holding
// admission for the entire carrier lifetime would prevent ordinary fallback.
func attachReservedCarrier(ctx context.Context, s *DirectSession, a localmesh.DirectCarrierAdmission, release func()) error {
	defer release()
	done := make(chan error, 1)
	go func() { done <- s.Attach(ctx, a) }()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			state := s.Status()
			if state.Established && state.BLEAlias == a.AttachmentAlias {
				release()
				return <-done
			}
		}
	}
}
