package twin

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

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
	compressed, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
	defer compressed.Close()
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
