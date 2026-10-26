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
	}{
		{name: "cached only"}, {name: "old address answers", probe: true}, {name: "both IDs already cached", probe: true, currentCached: true},
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
			fb.emit(t, wendyService(newID, "Pi 4", "pi4.local", "10.0.0.42", 50052))
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
