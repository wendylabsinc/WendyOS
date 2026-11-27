//go:build linux || darwin

package localmesh

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type lanHealthEntry struct {
	name      string
	transport string
	attempt   uint64
	status    CarrierStatus
}

// LAN workers restart independently as interface addresses come and go. Keep
// their readiness separate so a working Ethernet listener cannot hide a Wi-Fi
// bind failure, and a stale worker cannot clear its replacement's failure.
type lanCarrierHealth struct {
	mu              sync.Mutex
	entries         map[string]*lanHealthEntry
	hardware        map[string]bool
	transports      []string
	report          func(CarrierStatus)
	reportTransport func(string, CarrierStatus)
}

func (h *lanCarrierHealth) wanted(scan lanScan) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.entries == nil {
		h.entries = make(map[string]*lanHealthEntry)
	}
	h.hardware = scan.hardware
	wanted := make(map[string]bool, len(scan.interfaces))
	for _, iface := range scan.interfaces {
		key := iface.key()
		wanted[key] = true
		if h.entries[key] == nil {
			transport := iface.transport
			if transport == "usb" {
				transport = CarrierEthernet
			}
			h.entries[key] = &lanHealthEntry{name: iface.iface.Name, transport: transport}
		}
	}
	for key := range h.entries {
		if !wanted[key] {
			delete(h.entries, key)
		}
	}
	h.publish()
}

func (h *lanCarrierHealth) begin(key string) func(CarrierStatus) {
	h.mu.Lock()
	entry := h.entries[key]
	entry.attempt++
	attempt := entry.attempt
	// Retry is not readiness. Retain an error until the new worker has bound
	// its UDP QUIC listener and joined multicast successfully.
	entry.status.Ready = false
	h.publish()
	h.mu.Unlock()
	return func(status CarrierStatus) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.entries[key] != entry || entry.attempt != attempt {
			return
		}
		entry.status = status
		h.publish()
	}
}

// Called under mu to keep concurrent ready/failure notifications in order.
// The observer is a short in-memory state update, not a blocking operation.
func (h *lanCarrierHealth) publish() {
	if h.report != nil {
		h.report(h.status(""))
	}
	if h.reportTransport != nil {
		for _, transport := range h.transports {
			h.reportTransport(transport, h.status(transport))
		}
	}
}

func (h *lanCarrierHealth) status(transport string) CarrierStatus {
	keys := make([]string, 0, len(h.entries))
	for key, entry := range h.entries {
		if transport == "" || entry.transport == transport {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		if transport != "" && h.hardware != nil && !h.hardware[transport] {
			return CarrierStatus{NoHardware: true}
		}
		return CarrierStatus{Waiting: true, Detail: "waiting for an eligible LAN interface"}
	}
	var failed []error
	var ready []string
	for _, key := range keys {
		entry := h.entries[key]
		if entry.status.Ready {
			ready = append(ready, entry.name)
		}
		if entry.status.Err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", entry.name, entry.status.Err))
		}
	}
	detail := fmt.Sprintf("%d/%d eligible interfaces ready", len(ready), len(keys))
	if len(ready) != 0 {
		detail += ": " + strings.Join(ready, ", ")
	}
	return CarrierStatus{Ready: len(ready) > 0, Err: errors.Join(failed...), Detail: detail}
}
