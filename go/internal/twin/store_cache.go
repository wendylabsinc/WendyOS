package twin

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"unsafe"
)

// Cache only persisted JSON, never model assets, images or robot observations
// outside the room state. All access occurs under Store.mu. Readers receive
// detached values; private snapshots are immutable except derived floor tiles.
const roomCacheMaxEntries = 8
const roomCacheMaxBytes int64 = 32 << 20

type cachedRoom struct {
	room       Room
	info       os.FileInfo
	change     [2]int64
	used       uint64
	bytes      int64
	floor      []Point
	floorReady bool
	gzipState  []byte
}

func (s *Store) forgetRoom(id string) {
	id = strings.ToLower(id)
	if old := s.rooms[id]; old != nil {
		s.roomBytes -= old.bytes
		delete(s.rooms, id)
	}
}
func (s *Store) trimRooms() {
	for len(s.rooms) > roomCacheMaxEntries || s.roomBytes > roomCacheMaxBytes {
		oldest := ""
		var tick uint64
		for id, entry := range s.rooms {
			if oldest == "" || entry.used < tick {
				oldest, tick = id, entry.used
			}
		}
		s.forgetRoom(oldest)
	}
}
func (s *Store) roomSnapshot(id string) (*cachedRoom, error) {
	path, err := s.path(id, ".json")
	if err != nil {
		return nil, err
	}
	id = strings.ToLower(id)
	info, err := os.Stat(path)
	if err != nil {
		s.forgetRoom(id)
		return nil, err
	}
	change, hasChange := roomFileChange(info)
	s.roomTick++
	if old := s.rooms[id]; old != nil && hasChange && old.change == change && os.SameFile(old.info, info) && old.info.Size() == info.Size() && old.info.ModTime().Equal(info.ModTime()) {
		old.used = s.roomTick
		return old, nil
	}
	s.forgetRoom(id)
	bytes, info, err := readRoomSnapshot(path)
	if err != nil {
		return nil, err
	}
	change, hasChange = roomFileChange(info)
	var room Room
	if err = json.Unmarshal(bytes, &room); err != nil {
		return nil, err
	}
	entry := &cachedRoom{room: room, info: info, change: change, used: s.roomTick, bytes: roomRetainedBytes(room)}
	if hasChange && entry.bytes <= roomCacheMaxBytes {
		if s.rooms == nil {
			s.rooms = make(map[string]*cachedRoom)
		}
		s.rooms[id] = entry
		s.roomBytes += entry.bytes
		s.trimRooms()
	}
	return entry, nil
}
func (s *Store) roomFloor(id string, entry *cachedRoom) []Point {
	if !entry.floorReady {
		entry.floor = FloorTiles(entry.room.Points)
		entry.floorReady = true
		added := int64(cap(entry.floor)) * int64(unsafe.Sizeof(Point{}))
		entry.bytes += added
		if s.rooms[strings.ToLower(id)] == entry {
			s.roomBytes += added
			s.trimRooms()
		}
	}
	return entry.floor
}

func cloneRoom(room Room) Room {
	room.Site.Cameras = slices.Clone(room.Site.Cameras)
	for i := range room.Site.Cameras {
		room.Site.Cameras[i] = slices.Clone(room.Site.Cameras[i])
	}
	room.Model = slices.Clone(room.Model)
	room.VisualMap = slices.Clone(room.VisualMap)
	room.Points = slices.Clone(room.Points)
	room.LegacyPoints = slices.Clone(room.LegacyPoints)
	room.FloorTiles = slices.Clone(room.FloorTiles)
	room.PointLayers = maps.Clone(room.PointLayers)
	for name, points := range room.PointLayers {
		room.PointLayers[name] = slices.Clone(points)
	}
	room.References = maps.Clone(room.References)
	for serial, points := range room.References {
		room.References[serial] = slices.Clone(points)
	}
	room.Sources = maps.Clone(room.Sources)
	room.Waypoints = maps.Clone(room.Waypoints)
	if geometry := room.LocalizationGeometry; geometry != nil {
		copy := *geometry
		copy.Architecture = slices.Clone(geometry.Architecture)
		copy.Furniture = slices.Clone(geometry.Furniture)
		room.LocalizationGeometry = &copy
	}
	return room
}
func roomListItem(room Room) Room {
	room.Model = nil
	room.VisualMap = nil
	room.Points = nil
	room.PointLayers = nil
	room.LegacyPoints = nil
	room.References = nil
	room.LocalizationGeometry = nil
	room.Sources = nil
	return cloneRoom(room)
}

// Count backing-array capacity, keys and conservative map-entry overhead. The
// cache is also entry-bounded so tiny rooms cannot retain unbounded metadata.
func roomRetainedBytes(room Room) int64 {
	n := int64(2048) + int64(unsafe.Sizeof(cachedRoom{})) + int64(len(room.Site.ID)+len(room.Site.Name)+len(room.Site.ModelRevision))
	n += int64(cap(room.Model) + cap(room.VisualMap))
	n += int64(cap(room.Site.Cameras)) * int64(unsafe.Sizeof(json.RawMessage{}))
	for _, camera := range room.Site.Cameras {
		n += int64(cap(camera))
	}
	pointBytes := func(points []Point) int64 { return int64(cap(points)) * int64(unsafe.Sizeof(Point{})) }
	n += pointBytes(room.Points) + pointBytes(room.FloorTiles) + pointBytes(room.LegacyPoints)
	for key, points := range room.PointLayers {
		n += int64(len(key)+128) + pointBytes(points)
	}
	for key, points := range room.References {
		n += int64(len(key)+128) + int64(cap(points))*int64(unsafe.Sizeof(points[0]))
	}
	for key := range room.Sources {
		n += int64(len(key) + 128)
	}
	for key := range room.Waypoints {
		n += int64(len(key) + 128)
	}
	if geometry := room.LocalizationGeometry; geometry != nil {
		n += int64(unsafe.Sizeof(*geometry)) + int64(len(geometry.Frame)) + int64(cap(geometry.Architecture)+cap(geometry.Furniture))*8
	}
	return n
}

// Bind the decoded bytes to the opened file, not to a path that another
// process may have atomically replaced between stat and open.
func readRoomSnapshot(path string) ([]byte, os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	size := before.Size()
	if size < 0 || uint64(size) >= uint64(^uint(0)>>1) {
		return nil, nil, errors.New("room file is too large")
	}
	// One extra byte proves EOF without geometric read-buffer growth.
	data := make([]byte, int(size)+1)
	n, readErr := io.ReadFull(file, data)
	if readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		if readErr != nil {
			return nil, nil, readErr
		}
		return nil, nil, errors.New("room grew while being read; refresh before retrying")
	}
	if int64(n) != size {
		return nil, nil, errors.New("room changed while being read; refresh before retrying")
	}
	data = data[:n]
	after, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	beforeChange, _ := roomFileChange(before)
	afterChange, _ := roomFileChange(after)
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || beforeChange != afterChange {
		return nil, nil, errors.New("room changed while being read; refresh before retrying")
	}
	return data, after, nil
}
