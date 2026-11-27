package twin

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

var roomGzipWriters = sync.Pool{New: func() any { writer, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed); return writer }}

// Room snapshots contain dense point clouds and repeated floor vertices. Keep
// these transfers small over the authenticated container-exec connection.
func respondRoom(w http.ResponseWriter, r *http.Request, value any, err error) {
	// Localization mesh is an internal cache, not another large room asset to
	// resend on every dashboard/state refresh. Availability remains metadata.
	switch room := value.(type) {
	case Room:
		room.LocalizationGeometry = nil
		value = room
	case []Room:
		rooms := make([]Room, len(room))
		copy(rooms, room)
		for i := range rooms {
			rooms[i].LocalizationGeometry = nil
		}
		value = rooms
	}
	w.Header().Add("Vary", "Accept-Encoding")
	if err != nil || !acceptsGzip(strings.Join(r.Header.Values("Accept-Encoding"), ",")) {
		respond(w, value, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Encoding", "gzip")
	compressed := roomGzipWriters.Get().(*gzip.Writer)
	compressed.Reset(w)
	defer func() { _ = compressed.Close(); compressed.Reset(io.Discard); roomGzipWriters.Put(compressed) }()
	json.NewEncoder(compressed).Encode(value)
}

func acceptsGzip(header string) bool {
	wildcard := false
	for _, entry := range strings.Split(header, ",") {
		parts := strings.Split(entry, ";")
		encoding := strings.TrimSpace(parts[0])
		if !strings.EqualFold(encoding, "gzip") && encoding != "*" {
			continue
		}
		quality := 1.0
		for _, parameter := range parts[1:] {
			name, value, found := strings.Cut(strings.TrimSpace(parameter), "=")
			if found && strings.EqualFold(strings.TrimSpace(name), "q") {
				q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
				if err != nil || !(q >= 0 && q <= 1) {
					quality = 0
				} else {
					quality = q
				}
			}
		}
		if strings.EqualFold(encoding, "gzip") {
			return quality > 0
		}
		wildcard = quality > 0
	}
	return wildcard
}

// A room revision is immutable between Store writes. Reuse its gzip wire image
// for dashboard polling; it shares the same entry and byte budget as decoded
// snapshots, and every update or external file replacement invalidates both.
func (s *Store) respondState(w http.ResponseWriter, r *http.Request, id string) {
	if !acceptsGzip(strings.Join(r.Header.Values("Accept-Encoding"), ",")) {
		state, err := s.State(id)
		respondRoom(w, r, state, err)
		return
	}
	s.mu.Lock()
	entry, err := s.roomSnapshot(id)
	if err != nil {
		s.mu.Unlock()
		respondRoom(w, r, nil, err)
		return
	}
	payload := entry.gzipState
	if payload == nil {
		room := entry.room
		room.FloorTiles = s.roomFloor(id, entry)
		room.LocalizationGeometry = nil
		var buffer bytes.Buffer
		writer := roomGzipWriters.Get().(*gzip.Writer)
		writer.Reset(&buffer)
		err = json.NewEncoder(writer).Encode(room)
		closeErr := writer.Close()
		writer.Reset(io.Discard)
		roomGzipWriters.Put(writer)
		if err == nil {
			err = closeErr
		}
		if err != nil {
			s.mu.Unlock()
			respondRoom(w, r, nil, err)
			return
		}
		payload = buffer.Bytes()
		// Avoid retaining a huge response even when this request is allowed to
		// transmit it. Oversized rooms/derived floors were already evicted.
		if int64(cap(payload)) <= roomCacheMaxBytes/4 && s.rooms[strings.ToLower(id)] == entry {
			entry.gzipState = payload
			entry.bytes += int64(cap(payload))
			s.roomBytes += int64(cap(payload))
			s.trimRooms()
		}
	}
	s.mu.Unlock()
	w.Header().Add("Vary", "Accept-Encoding")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Encoding", "gzip")
	_, _ = w.Write(payload)
}
