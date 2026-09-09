//go:build linux

package central

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/wendylabsinc/wendy/go/internal/shared/ble/bluez"
)

// maxAttributeLen is the largest a GATT attribute value may be (Core
// Specification, Vol 3, Part F, 3.2.9). It bounds the read-long loop below.
const maxAttributeLen = 512

// defaultATTPayload is ATT_MTU-1 for the 23-byte default MTU: the most a single
// Read Response can carry before the MTU is negotiated upward. Used only as the
// truncation heuristic when BlueZ does not publish a characteristic's MTU.
const defaultATTPayload = 22

// DiscoverServices connects to the peripheral and indexes its GATT services and
// characteristics. Every other GATT method needs the index this builds.
//
// On Linux this is where the link comes up, unlike OpenL2CAP, which reaches the
// device through the kernel instead. The two coexist: the kernel reuses an ACL
// link BlueZ already established rather than opening a second one.
func (c *Connection) DiscoverServices(timeoutSeconds int) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	g, err := c.ensureSession(ctx)
	if err != nil {
		return err
	}
	if err := g.connect(ctx); err != nil {
		return err
	}
	if err := g.waitServicesResolved(ctx); err != nil {
		return err
	}
	return g.rebuildIndex(ctx)
}

// HasService reports whether discovery found the given service.
func (c *Connection) HasService(serviceUUID string) bool {
	if c.g == nil {
		return false
	}
	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	return c.g.svcSet[canonicalUUID(serviceUUID)]
}

// ListServices returns the discovered service UUIDs, comma-separated. The
// separator matches darwin's componentsJoinedByString so a message built from
// this reads the same on either platform.
func (c *Connection) ListServices() string {
	if c.g == nil {
		return ""
	}
	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	return strings.Join(c.g.services, ", ")
}

// ReadCharacteristic reads a characteristic's value.
func (c *Connection) ReadCharacteristic(serviceUUID, charUUID string) ([]byte, error) {
	entry, err := c.lookupChar(serviceUUID, charUUID)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), gattOpTimeout)
	defer cancel()
	obj := c.g.objFn(entry.path)

	value, err := readValueAt(ctx, obj, 0)
	if err != nil {
		return nil, wrapGATTError("reading "+charUUID, err)
	}

	// bluetoothd runs a read-long procedure only when an offset is given;
	// without one it issues a single ATT Read Request, which is capped at the
	// negotiated MTU. bluetoothd asks for a large MTU by default, so this loop
	// almost never runs — but a value truncated at the payload boundary would
	// be silent corruption, and that is worth a few lines.
	//
	// The test is on the length of the LAST chunk, not the total: a full chunk
	// is what says "there may be more", and the running total crosses the
	// boundary after the first append.
	for lastChunk := len(value); lastChunk > 0 && len(value) < maxAttributeLen && chunkLooksFull(lastChunk, entry.mtu); {
		more, err := readValueAt(ctx, obj, uint16(len(value)))
		if err != nil || len(more) == 0 {
			break
		}
		lastChunk = len(more)
		value = append(value, more...)
	}
	return value, nil
}

func readValueAt(ctx context.Context, obj bluez.Caller, offset uint16) ([]byte, error) {
	options := map[string]dbus.Variant{}
	if offset > 0 {
		options["offset"] = dbus.MakeVariant(offset)
	}
	call := obj.CallWithContext(ctx, bluez.GattCharIface+".ReadValue", 0, options)
	if call.Err != nil {
		return nil, call.Err
	}
	var value []byte
	if err := call.Store(&value); err != nil {
		return nil, fmt.Errorf("decoding value: %w", err)
	}
	return value, nil
}

// chunkLooksFull reports whether a read may have stopped at the ATT payload
// boundary rather than at the end of the value. Exact when the characteristic's
// MTU is known; before BlueZ 5.62 it is not published, and the default ATT
// payload is the best available guess.
func chunkLooksFull(n int, mtu uint16) bool {
	if mtu >= 2 {
		return n == int(mtu)-1
	}
	return n >= defaultATTPayload
}

// WriteCharacteristic writes with a response (ATT Write Request). bluetoothd
// falls back to prepare/execute writes on its own when the value exceeds the
// MTU, so long values need nothing here.
func (c *Connection) WriteCharacteristic(serviceUUID, charUUID string, data []byte) error {
	return c.writeChar(serviceUUID, charUUID, data, "request")
}

// WriteCharacteristicNoResponse writes without a response (ATT Write Command).
// A command cannot be long: a value over the MTU is rejected rather than split.
func (c *Connection) WriteCharacteristicNoResponse(serviceUUID, charUUID string, data []byte) error {
	return c.writeChar(serviceUUID, charUUID, data, "command")
}

func (c *Connection) writeChar(serviceUUID, charUUID string, data []byte, writeType string) error {
	entry, err := c.lookupChar(serviceUUID, charUUID)
	if err != nil {
		return err
	}
	if data == nil {
		// The "ay" signature needs an array, not nothing.
		data = []byte{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), gattOpTimeout)
	defer cancel()
	options := map[string]dbus.Variant{"type": dbus.MakeVariant(writeType)}
	call := c.g.objFn(entry.path).CallWithContext(ctx, bluez.GattCharIface+".WriteValue", 0, data, options)
	if call.Err != nil {
		// The flags go in the message on purpose. A write-without-response to a
		// characteristic that only accepts requests fails with an error that is
		// otherwise impossible to diagnose — and switching write type silently
		// would hide a real mismatch, which darwin does not do either.
		return wrapGATTError(
			fmt.Sprintf("writing %s as %s (characteristic flags: %s)", charUUID, writeType, flagList(entry.flags)),
			call.Err)
	}
	return nil
}

// Subscribe enables notifications for a characteristic.
func (c *Connection) Subscribe(serviceUUID, charUUID string) error {
	entry, err := c.lookupChar(serviceUUID, charUUID)
	if err != nil {
		return err
	}

	// The queue goes in before StartNotify, not after: bluetoothd can deliver
	// the first notification before the method reply lands, and the router
	// drops values for a path it has no queue for.
	c.g.mu.Lock()
	if _, exists := c.g.notify[entry.path]; !exists {
		c.g.notify[entry.path] = make(chan []byte, notifyQueueDepth)
	}
	c.g.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), gattOpTimeout)
	defer cancel()
	if call := c.g.objFn(entry.path).CallWithContext(ctx, bluez.GattCharIface+".StartNotify", 0); call.Err != nil {
		// Already notifying is what we wanted anyway.
		if !bluez.IsErrorName(call.Err, "org.bluez.Error.InProgress") {
			return wrapGATTError("subscribing to "+charUUID, call.Err)
		}
	}
	return nil
}

// WaitNotification returns the next queued notification for a subscribed
// characteristic, or an error if none arrives in time.
//
// Each characteristic has its own queue, so a notification on one can never
// wake a waiter on another — the failure mode the darwin backend still has.
func (c *Connection) WaitNotification(serviceUUID, charUUID string, timeoutSeconds int) ([]byte, error) {
	entry, err := c.lookupChar(serviceUUID, charUUID)
	if err != nil {
		return nil, err
	}

	c.g.mu.Lock()
	queue := c.g.notify[entry.path]
	c.g.mu.Unlock()
	if queue == nil {
		return nil, fmt.Errorf("not subscribed to %s — call Subscribe first", charUUID)
	}

	timer := time.NewTimer(time.Duration(timeoutSeconds) * time.Second)
	defer timer.Stop()
	select {
	case value := <-queue:
		return value, nil
	case <-c.g.lost:
		return nil, fmt.Errorf("%w while waiting for a notification on %s", ErrGATTDisconnected, charUUID)
	case <-timer.C:
		return nil, fmt.Errorf("no notification on %s within %ds", charUUID, timeoutSeconds)
	}
}

// lookupChar resolves a (service, characteristic) UUID pair against the index
// DiscoverServices built. A call before discovery and a call for a
// characteristic the device does not have are the same failure: nothing is in
// the index either way.
func (c *Connection) lookupChar(serviceUUID, charUUID string) (charEntry, error) {
	if c.g == nil {
		return charEntry{}, fmt.Errorf("%w: %s in %s — call DiscoverServices first",
			ErrGATTNotFound, charUUID, serviceUUID)
	}

	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	entry, ok := c.g.chars[charKey{
		service:        canonicalUUID(serviceUUID),
		characteristic: canonicalUUID(charUUID),
	}]
	if !ok {
		return charEntry{}, fmt.Errorf("%w: characteristic %s in service %s; device exposes [%s]",
			ErrGATTNotFound, charUUID, serviceUUID, strings.Join(c.g.services, ", "))
	}
	return entry, nil
}

func flagList(flags []string) string {
	if len(flags) == 0 {
		return "none reported"
	}
	return strings.Join(flags, ", ")
}
