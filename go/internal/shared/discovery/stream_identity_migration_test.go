package discovery

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestStreamRetiresCachedBootUUIDAfterEnrollment(t *testing.T) {
	const oldID = "16cc5e4b-ba64-47bc-b72c-1f426094fef7"
	const newID = "4c386c9d-9351-47b1-b176-19099e7e0fa2"
	for _, tc := range []struct {
		name                 string
		probe, currentCached bool
		renamed              bool
	}{
		{name: "cached only"}, {name: "old address answers", probe: true}, {name: "both IDs already cached", probe: true, currentCached: true},
		{name: "enrollment renames hostname", probe: true, renamed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "devices.json")
			entry := discoverycache.Entry{ID: oldID, DisplayName: "Pi 4", Hostname: "pi4.local", IP: "10.0.0.42", Port: 50052}
			entries := []discoverycache.Entry{entry}
			if tc.currentCached {
				entry.ID = newID
				entries = append(entries, entry)
			}
			seedCache(t, path, entries...)
			fb := newFakeBackend()
			useStreamSeams(t, fb.fn, cacheLoaderFor(path))
			opts := StreamOptions{UseCache: true}
			if tc.probe {
				opts.Prober = func(_ context.Context, d models.LANDevice) (models.LANDevice, error) {
					d.AgentVersion = "enrolled"
					return d, nil
				}
			}
			events, stop := startStream(t, opts)
			initial := len(entries)
			if tc.probe {
				initial *= 2
			}
			collectEvents(t, events, initial, 5*time.Second)
			host := "pi4.local"
			if tc.renamed {
				host = "enrolled-pi4.local"
			}
			fb.emit(t, wendyService(newID, "Pi 4", host, "10.0.0.42", 50052))
			n := 2
			if tc.probe && !tc.currentCached {
				n++
			}
			got := collectEvents(t, events, n, 5*time.Second)
			retired := false
			for _, ev := range got {
				if ev.Kind == LANRetracted && ev.Device.ID == oldID {
					retired = true
				}
			}
			if !retired {
				t.Fatalf("old boot UUID remained a live discovery row: %+v", got)
			}
			stop()
			cache, err := discoverycache.LoadFrom(path)
			if err != nil {
				t.Fatal(err)
			}
			fresh := cache.Fresh(time.Now())
			if len(fresh) != 1 || fresh[0].ID != newID {
				t.Fatalf("migration retained duplicate cache identities: %+v", fresh)
			}
		})
	}
}

func TestStreamPreservesDistinctLiveUUIDsAtTheSameHostname(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))
	seedCache(t, path, discoverycache.Entry{ID: "16cc5e4b-ba64-47bc-b72c-1f426094fef7", DisplayName: "box", Hostname: "box.local", IP: "10.0.0.42", Port: 50052})
	events, stop := startStream(t, StreamOptions{UseCache: true})
	collectEvents(t, events, 1, 5*time.Second)
	for _, id := range []string{"16cc5e4b-ba64-47bc-b72c-1f426094fef7", "4c386c9d-9351-47b1-b176-19099e7e0fa2"} {
		fb.emit(t, wendyService(id, "box", "box.local", "10.0.0.42", 50052))
		ev := collectEvents(t, events, 1, 5*time.Second)[0]
		if ev.Kind != LANFound || ev.Device.ID != id || ev.Supersedes != "" {
			t.Fatalf("conflated distinct live advertisements: %+v", ev)
		}
	}
	stop()
	cache, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if entries := cache.Fresh(time.Now()); len(entries) != 2 {
		t.Fatalf("lost a live identity: %+v", entries)
	}
}

// announcedOn is wendyService for one DNS-SD service instance heard on one
// interface, which is how every platform backend reports a sighting.
func announcedOn(iface, instance, id, display, hostname, ip string) MDNSService {
	svc := wendyService(id, display, hostname, ip, 50051)
	svc.InstanceName, svc.InterfaceName = instance, iface
	return svc
}

// liveLANRows replays what a surface keyed by cache identity shows after events.
func liveLANRows(events []LANEvent) map[string]bool {
	rows := make(map[string]bool)
	for _, ev := range events {
		if ev.Supersedes != "" {
			delete(rows, ev.Supersedes)
		}
		key := discoverycache.Key(ev.Device.ID, ev.Device.DisplayName)
		if ev.Kind == LANRetracted {
			delete(rows, key)
			continue
		}
		rows[key] = true
	}
	return rows
}

// Enrollment rewrites the id a board advertises while its service instance
// keeps its name. An open session that saw the boot id must end with one
// identity for the board, not two.
func TestStreamSupersedesAnIdentityItsServiceInstanceLeft(t *testing.T) {
	const bootID = "16cc5e4b-ba64-47bc-b72c-1f426094fef7"
	const certID = "4c386c9d-9351-47b1-b176-19099e7e0fa2"
	path := filepath.Join(t.TempDir(), "devices.json")
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))
	events, stop := startStream(t, StreamOptions{UseCache: true})

	fb.emit(t, announcedOn("en7", "kitchen-pi", bootID, "Kitchen Pi", "kitchen-pi.local", "fe80::a1b2%en7"))
	boot := collectEvents(t, events, 1, 5*time.Second)[0]
	if boot.Kind != LANFound || boot.Device.ID != bootID {
		t.Fatalf("boot identity not listed: %+v", boot)
	}

	fb.emit(t, announcedOn("en7", "kitchen-pi", certID, "Kitchen Pi", "kitchen-pi.local", "fe80::a1b2%en7"))
	enrolled := collectEvents(t, events, 1, 5*time.Second)[0]
	if enrolled.Kind != LANFound || enrolled.Device.ID != certID {
		t.Fatalf("enrolled identity not listed: %+v", enrolled)
	}
	if enrolled.Supersedes != bootID {
		t.Fatalf("Supersedes = %q; want the boot identity %q the same service instance announced before", enrolled.Supersedes, bootID)
	}
	expectQuiet(t, events, 200*time.Millisecond)
	if rows := liveLANRows([]LANEvent{boot, enrolled}); len(rows) != 1 || !rows[certID] {
		t.Fatalf("live rows = %v; want only the enrolled identity", rows)
	}

	stop()
	cache, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if fresh := cache.Fresh(time.Now()); len(fresh) != 1 || fresh[0].ID != certID {
		t.Fatalf("cache = %+v; want only the enrolled identity", fresh)
	}
}

// An identity goes only when every service instance that announced it has
// moved on. One still announced on another interface stays until that
// interface reports the change too.
func TestStreamKeepsAnIdentityStillAnnouncedOnAnotherInterface(t *testing.T) {
	const bootID = "16cc5e4b-ba64-47bc-b72c-1f426094fef7"
	const certID = "4c386c9d-9351-47b1-b176-19099e7e0fa2"
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, nil)
	events, _ := startStream(t, StreamOptions{})

	var seen []LANEvent
	next := func(svc MDNSService) LANEvent {
		t.Helper()
		fb.emit(t, svc)
		ev := collectEvents(t, events, 1, 5*time.Second)[0]
		seen = append(seen, ev)
		return ev
	}
	next(announcedOn("en0", "kitchen-pi", bootID, "Kitchen Pi", "kitchen-pi.local", "10.0.0.42"))
	next(announcedOn("en7", "kitchen-pi", bootID, "Kitchen Pi", "kitchen-pi.local", "fe80::a1b2%en7"))

	if ev := next(announcedOn("en7", "kitchen-pi", certID, "Kitchen Pi", "kitchen-pi.local", "fe80::a1b2%en7")); ev.Supersedes != "" {
		t.Fatalf("superseded %q while en0 still announces it: %+v", ev.Supersedes, ev)
	}
	if rows := liveLANRows(seen); len(rows) != 2 {
		t.Fatalf("live rows = %v; want both identities until en0 reports the change", rows)
	}

	if ev := next(announcedOn("en0", "kitchen-pi", certID, "Kitchen Pi", "kitchen-pi.local", "10.0.0.42")); ev.Supersedes != bootID {
		t.Fatalf("Supersedes = %q; want %q once every interface announces the new identity: %+v", ev.Supersedes, bootID, ev)
	}
	if rows := liveLANRows(seen); len(rows) != 1 || !rows[certID] {
		t.Fatalf("live rows = %v; want only the enrolled identity", rows)
	}
}

// Nothing but the same service instance on the same interface ties two
// identities together: not a shared instance name on another interface, not a
// shared hostname or address under another instance name, and not a sighting
// whose interface is unknown.
func TestStreamKeepsIdentitiesOfOtherServiceInstancesApart(t *testing.T) {
	const idA = "16cc5e4b-ba64-47bc-b72c-1f426094fef7"
	const idB = "4c386c9d-9351-47b1-b176-19099e7e0fa2"
	for _, tc := range []struct {
		name          string
		first, second MDNSService
	}{
		{
			name:   "same instance name on two interfaces",
			first:  announcedOn("en7", "wendyos", idA, "wendyos", "wendyos.local", "fe80::5741:1%en7"),
			second: announcedOn("en8", "wendyos", idB, "wendyos", "wendyos.local", "fe80::5741:1%en8"),
		},
		{
			name:   "two instances at one hostname and address",
			first:  announcedOn("en0", "box", idA, "box", "box.local", "10.0.0.42"),
			second: announcedOn("en0", "box (2)", idB, "box", "box.local", "10.0.0.42"),
		},
		{
			name:   "interface unknown",
			first:  announcedOn("", "box", idA, "box", "box.local", "10.0.0.42"),
			second: announcedOn("", "box", idB, "box", "box.local", "10.0.0.42"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := newFakeBackend()
			useStreamSeams(t, fb.fn, nil)
			events, _ := startStream(t, StreamOptions{})
			var seen []LANEvent
			for _, svc := range []MDNSService{tc.first, tc.second} {
				fb.emit(t, svc)
				ev := collectEvents(t, events, 1, 5*time.Second)[0]
				if ev.Kind != LANFound || ev.Supersedes != "" {
					t.Fatalf("conflated distinct advertisements: %+v", ev)
				}
				seen = append(seen, ev)
			}
			expectQuiet(t, events, 200*time.Millisecond)
			if rows := liveLANRows(seen); len(rows) != 2 {
				t.Fatalf("live rows = %v; want both identities", rows)
			}
		})
	}
}

// Two devices that both keep announcing through one service instance (a name
// conflict mDNS has not resolved yet) are both live. The one that was replaced
// gets its row back when it announces again, and neither replaces the other
// after that.
func TestStreamStopsSupersedingThroughAContestedServiceInstance(t *testing.T) {
	const idA = "16cc5e4b-ba64-47bc-b72c-1f426094fef7"
	const idB = "4c386c9d-9351-47b1-b176-19099e7e0fa2"
	path := filepath.Join(t.TempDir(), "devices.json")
	fb := newFakeBackend()
	useStreamSeams(t, fb.fn, cacheLoaderFor(path))
	events, stop := startStream(t, StreamOptions{UseCache: true})

	a := announcedOn("en0", "box", idA, "box", "box.local", "10.0.0.42")
	b := announcedOn("en0", "box", idB, "box", "box.local", "10.0.0.43")
	var seen []LANEvent
	for _, svc := range []MDNSService{a, b, a} {
		fb.emit(t, svc)
		seen = append(seen, collectEvents(t, events, 1, 5*time.Second)...)
	}
	if seen[1].Supersedes != idA {
		t.Fatalf("Supersedes = %q; want %q on the first change of identity: %+v", seen[1].Supersedes, idA, seen[1])
	}
	if back := seen[2]; back.Kind != LANFound || back.Device.ID != idA || back.Supersedes != "" {
		t.Fatalf("the replaced device announcing again must be listed again and replace nothing: %+v", back)
	}
	// Further announcements from either change nothing.
	for _, svc := range []MDNSService{b, a, b} {
		fb.emit(t, svc)
	}
	expectQuiet(t, events, 200*time.Millisecond)
	if rows := liveLANRows(seen); len(rows) != 2 {
		t.Fatalf("live rows = %v; want both devices", rows)
	}

	stop()
	cache, err := discoverycache.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if fresh := cache.Fresh(time.Now()); len(fresh) != 2 {
		t.Fatalf("cache = %+v; want both devices", fresh)
	}
}
