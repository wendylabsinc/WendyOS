//go:build linux

package localmesh

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

type lanHealthEntry struct {
	name    string
	attempt uint64
	status  CarrierStatus
}

// LAN workers restart independently as interface addresses come and go. Keep
// their readiness separate so a working Ethernet listener cannot hide a Wi-Fi
// bind failure, and a stale worker cannot clear its replacement's failure.
type lanCarrierHealth struct {
	mu      sync.Mutex
	entries map[string]*lanHealthEntry
	report  func(CarrierStatus)
}

func (h *lanCarrierHealth) wanted(interfaces []lanInterface) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.entries == nil {
		h.entries = make(map[string]*lanHealthEntry)
	}
	wanted := make(map[string]bool, len(interfaces))
	for _, iface := range interfaces {
		key := iface.key()
		wanted[key] = true
		if h.entries[key] == nil {
			h.entries[key] = &lanHealthEntry{name: iface.iface.Name}
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
	// its TCP listener and joined multicast successfully.
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
	if h.report == nil {
		return
	}
	keys := make([]string, 0, len(h.entries))
	for key := range h.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var failed []error
	ready := 0
	for _, key := range keys {
		entry := h.entries[key]
		if entry.status.Ready {
			ready++
		}
		if entry.status.Err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", entry.name, entry.status.Err))
		}
	}
	detail := fmt.Sprintf("%d/%d eligible interfaces ready", ready, len(keys))
	if len(keys) == 0 {
		detail = "waiting for an eligible LAN interface"
	}
	h.report(CarrierStatus{Ready: ready > 0, Err: errors.Join(failed...), Detail: detail})
}
