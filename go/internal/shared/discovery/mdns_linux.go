//go:build linux && !android

package discovery

import (
	"context"
	"errors"
)

// hashicorpFallbackFn is the fallback mdnsStreamBackend calls when Avahi is
// unavailable. A var, not a direct hashicorpStreamBackend call, so
// TestMdnsStreamBackendFallsBackOnAvahiUnavailable can pin the fallback
// wiring itself without also exercising hashicorp/mdns's real multicast
// query path (which does real network I/O and, as of hashicorp/mdns v1.0.6,
// carries its own pre-existing data race in client.go's Close/QueryContext
// interaction — unrelated to this backend, but real network I/O is also
// simply not what this unit test should depend on).
var hashicorpFallbackFn = hashicorpStreamBackend

// mdnsStreamBackend is the Linux implementation of the lanBackendFn and
// browseBackendFn seams (stream.go, mdns.go). It tries the no-child-process
// Avahi D-Bus backend
// (avahi_dbus_linux.go) first; when the Avahi daemon itself is unreachable
// (errAvahiUnavailable — no system bus, or nothing answers as
// org.freedesktop.Avahi) it falls back to the hashicorp/mdns streaming
// backend (backend_hashicorp.go). Any other avahi error is returned as-is,
// so the streaming engine restarts this backend instead of silently
// downgrading a daemon that started browsing and then failed.
func mdnsStreamBackend(ctx context.Context, serviceType string, emit func(MDNSService)) error {
	err := avahiStreamBackend(ctx, serviceType, emit)
	if errors.Is(err, errAvahiUnavailable) {
		logMDNSQueryErr("avahi-dbus", err) // WENDY_MDNS_DEBUG visibility
		return hashicorpFallbackFn(ctx, serviceType, emit)
	}
	return err
}
