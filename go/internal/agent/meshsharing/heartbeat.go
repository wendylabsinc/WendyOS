package meshsharing

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

type heartbeatFailure struct {
	phase string
	err   error
}

func (e *heartbeatFailure) Error() string { return fmt.Sprintf("gateway %s: %v", e.phase, e.err) }
func (e *heartbeatFailure) Unwrap() error { return e.err }

// exchangeLeaseHeartbeats permits only one outstanding request. Independent
// writers can queue requests behind a slow ACK; TCP then delivers a burst that
// the gateway correctly refuses as too frequent. A minimum wait after the ACK
// preserves the gateway's rate limit even when a radio delays delivery, without
// adding a full nominal period to a slow round trip.
// The owner closes conn to interrupt I/O when stop is closed.
func exchangeLeaseHeartbeats(conn net.Conn, stop <-chan struct{}, interval, minInterval, timeout time.Duration) error {
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		sent := time.Now()
		if err := conn.SetDeadline(sent.Add(timeout)); err != nil {
			return &heartbeatFailure{"heartbeat-deadline", err}
		}
		if err := writeControl(conn, []byte{leaseHeartbeat}); err != nil {
			return &heartbeatFailure{"heartbeat-write", err}
		}
		var ack [1]byte
		if _, err := io.ReadFull(conn, ack[:]); err != nil {
			return &heartbeatFailure{"heartbeat-ack-read", err}
		}
		if ack[0] != leaseHeartbeat {
			return &heartbeatFailure{"heartbeat-ack-validation", errors.New("invalid gateway heartbeat acknowledgement")}
		}
		wait := time.NewTimer(max(time.Until(sent.Add(interval)), minInterval))
		select {
		case <-stop:
			wait.Stop()
			return nil
		case <-wait.C:
		}
	}
}
