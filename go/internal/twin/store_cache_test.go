package twin

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

func cacheTestRoom(t *testing.T, store *Store) Room {
	t.Helper()
	saved, err := store.Put(room())
	if err != nil {
		t.Fatal(err)
	}
	saved.Site.Cameras = []json.RawMessage{json.RawMessage(`{"name":"camera"}`)}
	saved.Points = []Point{{X: 1}}
	saved.PointLayers = map[string][]Point{"scan": {{X: 2}}}
	saved.LegacyPoints = []Point{{X: 3}}
	saved.References = map[string][]sitenav.Surface{"robot": {{Position: sitenav.Vertex{X: 4}}}}
	saved.Sources = map[string]uint64{"scan": 5}
	saved.Waypoints = map[string]Waypoint{"desk": {X: 7}}
	saved.LocalizationGeometry = &RoomLocalizationGeometry{Frame: saved.Site.ID + "/" + saved.Site.ModelRevision, Architecture: []float64{8}, Furniture: []float64{9}}
	if err := store.write(saved); err != nil {
		t.Fatal(err)
	}
	path, _ := store.path(saved.Site.ID, ".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, supported := roomFileChange(info); !supported {
		t.Skip("retained room snapshots require a filesystem change stamp")
	}
	return saved
}
func TestRoomCacheReturnsDetachedStateAndMetadata(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	saved := cacheTestRoom(t, store)
	frame := saved.Site.ID + "/" + saved.Site.ModelRevision
	original, _ := store.Frame(frame)
	before, _ := json.Marshal(original)
	original.Site.Cameras[0][2] = 'X'
	original.Points[0].X = 99
	original.PointLayers["scan"][0].X = 99
	original.LegacyPoints[0].X = 99
	original.References["robot"][0].Position.X = 99
	original.Sources["scan"] = 99
	original.Waypoints["desk"] = Waypoint{X: 99}
	original.LocalizationGeometry.Architecture[0] = 99
	original.LocalizationGeometry.Furniture[0] = 99
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	list[0].Site.Cameras[0][2] = 'Y'
	list[0].Waypoints["desk"] = Waypoint{X: 88}
	again, _ := store.Frame(frame)
	after, _ := json.Marshal(again)
	if !bytes.Equal(before, after) {
		t.Fatal("caller mutation escaped into cached room")
	}
	if len(store.rooms) != 1 {
		t.Fatal("room was not cached")
	}
}
func TestRoomCacheInvalidatesOwnWritesAndExternalAtomicReplacement(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	saved := cacheTestRoom(t, store)
	frame := saved.Site.ID + "/" + saved.Site.ModelRevision
	_, _ = store.Frame(frame)
	if err := store.SaveWaypoint(frame, "door", Waypoint{X: 12}); err != nil {
		t.Fatal(err)
	}
	changed, err := store.Frame(frame)
	if err != nil || changed.Waypoints["door"].X != 12 {
		t.Fatalf("write cache stale: %+v %v", changed.Waypoints, err)
	}
	path, _ := store.path(saved.Site.ID, ".json")
	info, _ := os.Stat(path)
	raw, _ := os.ReadFile(path)
	replacement := bytes.Replace(raw, []byte(`"door":{"x":12`), []byte(`"door":{"x":34`), 1)
	if bytes.Equal(raw, replacement) {
		t.Fatal("fixture did not replace destination")
	}
	next := path + ".replacement"
	if err := os.WriteFile(next, replacement, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(next, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	changed, err = store.Frame(frame)
	if err != nil || changed.Waypoints["door"].X != 34 {
		t.Fatal("same-size same-time atomic replacement was not detected")
	}
	// Restore mtime after an in-place rewrite: ctime still invalidates it.
	info, _ = os.Stat(path)
	inPlace := bytes.Replace(replacement, []byte(`"door":{"x":34`), []byte(`"door":{"x":56`), 1)
	if err := os.WriteFile(path, inPlace, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	changed, err = store.Frame(frame)
	if err != nil || changed.Waypoints["door"].X != 56 {
		t.Fatal("same-inode same-size restored-mtime rewrite was not detected")
	}
	if err := os.WriteFile(path, []byte(`{broken`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Frame(frame); err == nil || len(store.rooms) != 0 {
		t.Fatal("corruption served an old cached room")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Frame(frame); err == nil {
		t.Fatal("deleted room served from cache")
	}
}
func TestRoomCacheHasEntryAndByteBounds(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	for i := 0; i < roomCacheMaxEntries+2; i++ {
		saved := cacheTestRoom(t, store)
		_, _ = store.State(saved.Site.ID)
	}
	if len(store.rooms) > roomCacheMaxEntries || store.roomBytes > roomCacheMaxBytes {
		t.Fatal("cache exceeds limits")
	}
	// Account backing capacity, not only visible length; oversized rooms are
	// valid for reading but must never enter the retained cache.
	room := Room{Points: make([]Point, 1, roomCacheMaxBytes/24+1)}
	if roomRetainedBytes(room) <= roomCacheMaxBytes {
		t.Fatal("backing capacity is not counted")
	}
	// Exercise byte eviction without a large runtime allocation in the test.
	store.mu.Lock()
	for _, entry := range store.rooms {
		store.roomBytes -= entry.bytes
		entry.bytes = roomCacheMaxBytes / 2
		store.roomBytes += entry.bytes
	}
	store.trimRooms()
	store.mu.Unlock()
	if len(store.rooms) > 2 || store.roomBytes > roomCacheMaxBytes {
		t.Fatal("byte limit failed")
	}
}
func TestRoomCacheFloorDerivationIsDetachedAndInvalidated(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	saved := cacheTestRoom(t, store)
	saved.Points = nil
	for x := 0; x < 8; x++ {
		for z := 0; z < 8; z++ {
			saved.Points = append(saved.Points, Point{X: (float64(x) + .5) / 10, Z: (float64(z) + .5) / 10})
		}
	}
	if err := store.write(saved); err != nil {
		t.Fatal(err)
	}
	first, err := store.State(saved.Site.ID)
	if err != nil || len(first.FloorTiles) == 0 {
		t.Fatalf("floor fixture %v", err)
	}
	expected := slices.Clone(first.FloorTiles)
	first.FloorTiles[0].X = 999
	next, _ := store.State(saved.Site.ID)
	if !slices.Equal(expected, next.FloorTiles) {
		t.Fatal("derived floor was mutated")
	}
	saved.Points = nil
	if err := store.write(saved); err != nil {
		t.Fatal(err)
	}
	next, _ = store.State(saved.Site.ID)
	if len(next.FloorTiles) != 0 {
		t.Fatal("floor cache survived new point cloud")
	}
}

func TestRoomStateGzipCacheMatchesDetachedStateAndInvalidates(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	saved := cacheTestRoom(t, store)
	handler := (&Server{Store: store}).Handler()
	get := func() (*httptest.ResponseRecorder, Room) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/rooms/"+saved.Site.ID+"/state", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		reader, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		var room Room
		if err = json.NewDecoder(reader).Decode(&room); err != nil {
			t.Fatal(err)
		}
		return w, room
	}
	first, got := get()
	originalBody := slices.Clone(first.Body.Bytes())
	expected, _ := store.State(saved.Site.ID)
	expected.LocalizationGeometry = nil
	a, _ := json.Marshal(expected)
	b, _ := json.Marshal(got)
	if !bytes.Equal(a, b) {
		t.Fatal("cached wire state changed public fields")
	}
	if got.LocalizationGeometry != nil {
		t.Fatal("localization mesh leaked into response")
	}
	second, _ := get()
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatal("unchanged gzip state was not reused")
	}
	entry := store.rooms[saved.Site.ID]
	if len(entry.gzipState) == 0 || entry.bytes < int64(cap(entry.gzipState)) {
		t.Fatal("wire image not charged to bounded cache")
	}
	if err := store.SaveWaypoint(saved.Site.ID+"/"+saved.Site.ModelRevision, "new door", Waypoint{X: 12}); err != nil {
		t.Fatal(err)
	}
	_, got = get()
	if got.Waypoints["new door"].X != 12 {
		t.Fatal("state payload survived a write")
	}
	if !bytes.Equal(first.Body.Bytes(), originalBody) {
		t.Fatal("old in-flight payload was mutated by update")
	}
}
func TestRoomStateCacheConcurrentWritersAndReaders(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	saved := cacheTestRoom(t, store)
	handler := (&Server{Store: store}).Handler()
	frame := saved.Site.ID + "/" + saved.Site.ModelRevision
	var work sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		work.Add(1)
		go func() {
			defer work.Done()
			for i := 0; i < 30; i++ {
				request := httptest.NewRequest(http.MethodGet, "/v1/rooms/"+saved.Site.ID+"/state", nil)
				request.Header.Set("Accept-Encoding", "gzip")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				reader, err := gzip.NewReader(bytes.NewReader(response.Body.Bytes()))
				if err != nil {
					t.Error(err)
					return
				}
				var room Room
				err = json.NewDecoder(reader).Decode(&room)
				reader.Close()
				if err != nil {
					t.Error(err)
					return
				}
				room.Waypoints["desk"] = Waypoint{X: -999}
			}
		}()
	}
	for i := 0; i < 15; i++ {
		if err := store.SaveWaypoint(frame, "progress", Waypoint{X: float64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	work.Wait()
	latest, _ := store.Frame(frame)
	if latest.Waypoints["desk"].X != 7 || latest.Waypoints["progress"].X != 14 {
		t.Fatal("concurrent response polluted room state")
	}
}
