//go:build linux

package scan

import (
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/bluez"
)

// BlueZ DuplicateData=true reports ServiceData on each actual discovery. The
// background mesh scanner records those signals, never a GetManagedObjects
// cache read, as freshness. See org.bluez.Adapter SetDiscoveryFilter docs.
const linuxObservationLimit = 1024

type linuxObservations struct {
	mu     sync.Mutex
	byPath map[dbus.ObjectPath]BLEDeviceInfo
}

func addressFromDevicePath(adapter string, path dbus.ObjectPath) string {
	suffix, ok := strings.CutPrefix(string(path), adapter+"/dev_")
	if !ok {
		return ""
	}
	address := strings.ReplaceAll(suffix, "_", ":")
	mac, err := net.ParseMAC(address)
	if err != nil || len(mac) != 6 {
		return ""
	}
	return strings.ToUpper(mac.String())
}
func serviceBytes(v dbus.Variant) map[string][]byte {
	result := make(map[string][]byte)
	parts, ok := v.Value().(map[string]dbus.Variant)
	if !ok {
		return result
	}
	for key, value := range parts {
		data, ok := value.Value().([]byte)
		if !ok || len(data) > 512 || len(result) >= 8 {
			continue
		}
		key = CanonicalUUID(key)
		if key != "" {
			result[key] = append([]byte(nil), data...)
		}
	}
	return result
}
func (o *linuxObservations) observe(adapter string, signal *dbus.Signal, now time.Time) {
	if signal == nil || signal.Name != bluez.PropsChangedName || len(signal.Body) < 2 {
		return
	}
	iface, ok := signal.Body[0].(string)
	if !ok || iface != bluez.DeviceIface {
		return
	}
	address := addressFromDevicePath(adapter, signal.Path)
	if address == "" {
		return
	}
	changed, ok := signal.Body[1].(map[string]dbus.Variant)
	if !ok {
		return
	}
	service, hasService := changed["ServiceData"]
	_, hasManufacturer := changed["ManufacturerData"]
	invalidated := false
	if len(signal.Body) > 2 {
		if fields, ok := signal.Body[2].([]string); ok {
			for _, field := range fields {
				if field == "ServiceData" {
					invalidated = true
				}
			}
		}
	}
	if !hasService && !hasManufacturer && !invalidated {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.byPath == nil {
		o.byPath = make(map[dbus.ObjectPath]BLEDeviceInfo)
	}
	entry, exists := o.byPath[signal.Path]
	if !exists && len(o.byPath) >= linuxObservationLimit {
		var oldest dbus.ObjectPath
		for key, device := range o.byPath {
			if oldest == "" || device.LastSeen.Before(o.byPath[oldest].LastSeen) || (device.LastSeen.Equal(o.byPath[oldest].LastSeen) && key < oldest) {
				oldest = key
			}
		}
		delete(o.byPath, oldest)
	}
	entry.Address = address
	if hasService || hasManufacturer {
		entry.LastSeen = now
	}
	if hasService {
		entry.ServiceData = serviceBytes(service)
		entry.ServiceDataSeen = now
		entry.ServiceUUIDs = nil
		for key := range entry.ServiceData {
			entry.ServiceUUIDs = append(entry.ServiceUUIDs, key)
		}
		sort.Strings(entry.ServiceUUIDs)
	}
	if invalidated {
		entry.ServiceData = map[string][]byte{}
		entry.ServiceDataSeen = now
		entry.ServiceUUIDs = nil
	}
	if rssi, ok := changed["RSSI"]; ok {
		if n, ok := rssi.Value().(int16); ok {
			entry.RSSI = int(n)
		}
	}
	o.byPath[signal.Path] = entry
}
func (o *linuxObservations) device(path dbus.ObjectPath) BLEDeviceInfo {
	o.mu.Lock()
	defer o.mu.Unlock()
	return cloneDevice(o.byPath[path])
}
func (o *linuxObservations) snapshot(limit int) []BLEDeviceInfo {
	o.mu.Lock()
	defer o.mu.Unlock()
	rows := make([]BLEDeviceInfo, 0, len(o.byPath))
	for _, device := range o.byPath {
		rows = append(rows, cloneDevice(device))
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].LastSeen != rows[j].LastSeen {
			return rows[i].LastSeen.After(rows[j].LastSeen)
		}
		return rows[i].Address < rows[j].Address
	})
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

// godbus's default signal handler spawns deferred-delivery goroutines when its
// channel fills; the sequential alternative has an unbounded private queue.
// This dedicated connection instead ingests bounded observations synchronously
// in arrival order. It neither blocks on I/O nor retains a signal backlog.
type linuxAdvertisementHandler struct {
	mu           sync.Mutex
	adapter      string
	closed       bool
	observations *linuxObservations
}

func (h *linuxAdvertisementHandler) setAdapter(adapter string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.adapter = adapter
}
func (h *linuxAdvertisementHandler) DeliverSignal(_, _ string, signal *dbus.Signal) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed && h.adapter != "" {
		h.observations.observe(h.adapter, signal, time.Now())
	}
}
func (h *linuxAdvertisementHandler) Terminate() { h.mu.Lock(); defer h.mu.Unlock(); h.closed = true }
