package twin

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
)

func TestRoomResponseCompressionNegotiation(t *testing.T) {
	for _, test := range []struct {
		header string
		gzip   bool
	}{
		{"", false}, {"identity", false}, {"br, gzip", true},
		{"gzip;q=0", false}, {"gzip; q=0.5", true}, {"GZIP", true},
		{"*", true}, {"*;q=0", false}, {"*, gzip;q=0", false},
		{"gzip;q=0, *", false}, {"gzip;q=invalid", false},
		{"gzip;q=NaN", false}, {"gzip;q=2", false},
	} {
		t.Run(test.header, func(t *testing.T) {
			if got := acceptsGzip(test.header); got != test.gzip {
				t.Fatalf("acceptsGzip(%q) = %v", test.header, got)
			}
		})
	}
}

func TestRoomResponsesCompressedWithoutChangingJSON(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := room()
	if _, err := store.Put(r); err != nil {
		t.Fatal(err)
	}
	var points []Point
	for x := 0; x < 40; x++ {
		for z := 0; z < 40; z++ {
			points = append(points, Point{X: float64(x) / 10, Z: float64(z) / 10})
		}
	}
	if err := store.Contribute(r.Site.ID, Contribution{Source: "test", Sequence: 1, ModelRevision: r.Site.ModelRevision, Points: points}); err != nil {
		t.Fatal(err)
	}
	handler := (&Server{Store: store, Context: context.Background()}).Handler()
	for _, path := range []string{"/v1/rooms", "/v1/rooms/" + r.Site.ID, "/v1/rooms/" + r.Site.ID + "/state"} {
		t.Run(path, func(t *testing.T) {
			plain := httptest.NewRecorder()
			handler.ServeHTTP(plain, httptest.NewRequest("GET", path, nil))
			compressed := httptest.NewRecorder()
			req := httptest.NewRequest("GET", path, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			handler.ServeHTTP(compressed, req)
			if plain.Code != 200 || compressed.Code != 200 || plain.Header().Get("Content-Encoding") != "" || compressed.Header().Get("Content-Encoding") != "gzip" {
				t.Fatalf("plain status/headers: %d %v; compressed: %d %v", plain.Code, plain.Header(), compressed.Code, compressed.Header())
			}
			if compressed.Header().Get("Vary") != "Accept-Encoding" || compressed.Header().Get("Content-Type") != "application/json" {
				t.Fatal(compressed.Header())
			}
			reader, err := gzip.NewReader(compressed.Body)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			decoded, err := io.ReadAll(reader)
			if err != nil || !json.Valid(decoded) {
				t.Fatalf("compressed snapshot changed: %v", err)
			}
			if path == "/v1/rooms" {
				if !bytes.Equal(decoded, plain.Body.Bytes()) {
					t.Fatal("compressed room list changed")
				}
				return
			}
			var original, restored Room
			if err := json.Unmarshal(plain.Body.Bytes(), &original); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(decoded, &restored); err != nil {
				t.Fatal(err)
			}
			// Floor vertices are regenerated from a map on each request, so
			// their ordering may differ without changing the geometry.
			pointOrder := func(a, b Point) int {
				if c := cmp.Compare(a.X, b.X); c != 0 {
					return c
				}
				if c := cmp.Compare(a.Y, b.Y); c != 0 {
					return c
				}
				return cmp.Compare(a.Z, b.Z)
			}
			slices.SortFunc(original.FloorTiles, pointOrder)
			slices.SortFunc(restored.FloorTiles, pointOrder)
			if !reflect.DeepEqual(original, restored) {
				t.Fatal("compressed snapshot changed")
			}
		})
	}
	// Errors retain their existing status and readable body.
	req := httptest.NewRequest("GET", "/v1/rooms/missing/state", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != 400 || response.Header().Get("Content-Encoding") != "" {
		t.Fatalf("missing room: %d %v", response.Code, response.Header())
	}
}
