//go:build linux

package bleprovider

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// Keep the provider's own transport so cancellation can interrupt a blocked
// godbus write. Conn.Close alone waits for that write's output lock. BlueZ is a
// local system service; support the usual Unix path/abstract bus addresses and
// alternatives without exposing or closing any other client's connection.
func connectProviderBus(ctx context.Context) (*dbus.Conn, func() error, error) {
	address := os.Getenv("DBUS_SYSTEM_BUS_ADDRESS")
	if address == "" {
		address = "unix:path=/var/run/dbus/system_bus_socket"
	}
	return connectProviderBusAddress(ctx, address)
}

func connectProviderBusAddress(ctx context.Context, address string) (*dbus.Conn, func() error, error) {
	var last error
	for _, alternative := range strings.Split(address, ";") {
		socket, err := providerBusSocket(alternative)
		if err != nil {
			last = err
			continue
		}
		dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		raw, err := (&net.Dialer{}).DialContext(dialCtx, "unix", socket)
		if err != nil {
			cancel()
			last = err
			continue
		}
		// Bound authentication/Hello as well as dialing. ConnectUnix preserves the
		// native Unix transport (including FD passing), unlike generic NewConn.
		deadline, _ := dialCtx.Deadline()
		_ = raw.SetDeadline(deadline)
		stop := context.AfterFunc(dialCtx, func() { _ = raw.Close() })
		bus, err := dbus.ConnectUnix(raw.(*net.UnixConn))
		stopped := stop()
		cancel()
		if err != nil || !stopped {
			_ = raw.Close()
			if bus != nil {
				_ = bus.Close()
			}
			if err == nil {
				err = context.DeadlineExceeded
			}
			last = err
			continue
		}
		if err = raw.SetDeadline(time.Time{}); err != nil {
			_ = raw.Close()
			_ = bus.Close()
			return nil, nil, err
		}
		return bus, raw.Close, nil
	}
	return nil, nil, fmt.Errorf("connect local BlueZ bus: %w", last)
}

func providerBusSocket(address string) (string, error) {
	transport, values, ok := strings.Cut(address, ":")
	if !ok || transport != "unix" {
		return "", fmt.Errorf("BlueZ requires a local Unix bus address")
	}
	var path, abstract string
	for _, pair := range strings.Split(values, ",") {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return "", fmt.Errorf("invalid Unix bus address")
		}
		value, err := dbus.UnescapeBusAddressValue(value)
		if err != nil {
			return "", err
		}
		switch key {
		case "path":
			path = value
		case "abstract":
			abstract = value
		}
	}
	if (path == "") == (abstract == "") {
		return "", fmt.Errorf("Unix bus requires exactly one path or abstract name")
	}
	if abstract != "" {
		return "@" + abstract, nil
	}
	return path, nil
}

// A context cancels godbus's pending reply, but cannot interrupt a synchronous
// transport send. Allow orderly loop joining/unregistration, then release any
// blocked send on this provider's socket. The returned function joins this
// watchdog; it is called after the final raw/bus close on every Run exit.
func guardProviderBusShutdown(ctx context.Context, grace time.Duration, closeTransport func() error) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-stop:
			return
		case <-ctx.Done():
		}
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-stop:
			return
		case <-timer.C:
			_ = closeTransport()
		}
	}()
	return func() { close(stop); <-done }
}
