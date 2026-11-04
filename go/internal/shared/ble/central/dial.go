package central

import (
	"context"
	"fmt"
	"net"
)

// DialL2CAPContext opens an explicitly selected LE credit-based channel. Its
// setup context stops owning the stream on return; callers own cancellation
// during TLS and the returned connection's lifetime. Native GAP and channel
// calls each have a finite ten-second budget even on platforms without a
// native context API. No GATT service or device provisioning is assumed.
func DialL2CAPContext(ctx context.Context, address string, psm uint16) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if psm < 0x80 || psm > 0xff {
		return nil, fmt.Errorf("BLE mesh PSM must be 0x0080 through 0x00ff")
	}
	type result struct {
		stream net.Conn
		err    error
	}
	completed := make(chan result)
	go func() {
		connection, err := Connect(address, 10)
		if err != nil {
			select {
			case completed <- result{err: err}:
			case <-ctx.Done():
			}
			return
		}
		// Keep the native handle owned by this worker until setup returns. Some
		// platforms cannot safely free it concurrently with OpenL2CAP; cancellation
		// releases the caller immediately and this finite worker then cleans up.
		if err = ctx.Err(); err == nil {
			err = connection.OpenL2CAP(psm, 10)
		}
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			connection.Close()
			select {
			case completed <- result{err: fmt.Errorf("open BLE mesh channel: %w", err)}:
			case <-ctx.Done():
			}
			return
		}
		stream := NewL2CAPStream(connection)
		select {
		case completed <- result{stream: stream}:
		case <-ctx.Done():
			stream.Close()
		}
	}()
	select {
	case result := <-completed:
		return result.stream, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
