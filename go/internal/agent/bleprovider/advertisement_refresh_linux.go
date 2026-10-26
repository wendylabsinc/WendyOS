//go:build linux

package bleprovider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
)

const advertisementRefreshInterval = time.Second

// Connecting consumes a connectable advertising set on some controllers. BlueZ
// refreshes its existing client instance on ServiceData PropertiesChanged, even
// when the value is unchanged. Request only after an inbound peer authenticates
// and is admitted; failed TLS attempts already close their connection.
func (r *runtime) requestAdvertisementRefresh() {
	select {
	case r.advertisementRefresh <- struct{}{}:
	default: // One pending request suffices, including while a pulse is in flight.
	}
}

func runAdvertisementRefresh(ctx context.Context, requests <-chan struct{}, interval time.Duration, pulse func(context.Context) error) error {
	var next time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-requests:
		}
		if delay := time.Until(next); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
		}
		// Merge requests received while waiting for the rate limit. A request
		// arriving during the pulse remains pending for the next interval.
		select {
		case <-requests:
		default:
		}
		if ctx.Err() != nil {
			return nil
		}
		if err := pulse(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// A failed refresh must never take down the provider: BlueZ
			// keeps serving the last-good advertisement parameters, so a
			// transient failure (controller busy, D-Bus race) only delays
			// the update. Only a changed BlueZ owner is fatal, matching
			// the scan loop. (Proven relevant: Intel controllers report
			// EBUSY on advertising reconfig under load; a fatal return
			// here would drop every established link at once.)
			if errors.Is(err, errBlueZOwnerChanged) {
				return fmt.Errorf("refreshing owned BLE advertisement: %w", err)
			}
		}
		next = time.Now().Add(interval)
	}
}

// Emit has no cancellable send in godbus. A blocked send must not prevent Run
// from joining its loops before unregistration. Closing only this provider's
// private bus transport releases the write and invalidates its registrations.
// This is a failure path, never a routine adapter/discovery reset.
func boundedAdvertisementEmit(ctx context.Context, emit func() error, closeBus func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- emit() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = closeBus()
		<-done
		return ctx.Err()
	}
}

func refreshOwnedAdvertisement(ctx context.Context, bus *dbus.Conn, closeTransport func() error, owner, uuid string, payload []byte) error {
	query, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// No registration, power, interval, or other exported property is changed.
	// BlueZ watches this exact sender/path; other clients' instances are untouched.
	changed := map[string]dbus.Variant{"ServiceData": dbus.MakeVariant(map[string]dbus.Variant{uuid: dbus.MakeVariant(payload)})}
	return boundedAdvertisementEmit(query, func() error {
		current, err := bluezOwner(query, bus)
		if err != nil {
			return err
		}
		if err := verifyBlueZOwner(owner, current); err != nil {
			return err
		}
		if err := query.Err(); err != nil {
			return err
		}
		return bus.Emit(advertPath, "org.freedesktop.DBus.Properties.PropertiesChanged", advertInterface, changed, []string{})
	}, closeTransport)
}
