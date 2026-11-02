package meshsharing

import (
	"errors"
	"io"
	"net"
	"time"
)

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
			return err
		}
		if err := writeControl(conn, []byte{leaseHeartbeat}); err != nil {
			return err
		}
		var ack [1]byte
		if _, err := io.ReadFull(conn, ack[:]); err != nil {
			return err
		}
		if ack[0] != leaseHeartbeat {
			return errors.New("invalid gateway heartbeat acknowledgement")
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
