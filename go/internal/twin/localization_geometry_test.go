package twin

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
)

func localizationGeometryFixture(frame string) twinrobot.SiteFusionRequest {
	vertices := [][3]float64{{0, 0, 0}, {4, 0, 0}, {4, 0, -5}, {0, 0, 0}, {4, 0, -5}, {0, 0, -5}}
	for _, wall := range [][4]float64{{0, 0, 4, 0}, {4, 0, 4, -5}, {4, -5, 0, -5}, {0, -5, 0, 0}} {
		a, b, c, d := [3]float64{wall[0], 0, wall[1]}, [3]float64{wall[2], 0, wall[3]}, [3]float64{wall[2], 2, wall[3]}, [3]float64{wall[0], 2, wall[1]}
		vertices = append(vertices, a, b, c, a, c, d)
	}
	input := twinrobot.SiteFusionRequest{Frame: frame}
	for _, v := range vertices {
		input.Architecture = append(input.Architecture, v[:]...)
	}
	return input
}

func TestLocalizationGeometryCachePersistsAndProtectsFrame(t *testing.T) {
	directory := t.TempDir()
	store, _ := NewStore(directory)
	saved, err := store.Put(room())
	if err != nil {
		t.Fatal(err)
	}
	frame := saved.Site.ID + "/" + saved.Site.ModelRevision
	input := localizationGeometryFixture(frame)
	input.Explore = true
	if err := store.SaveLocalizationGeometry(input); err != nil {
		t.Fatal(err)
	}
	cached, err := store.Frame(frame)
	if err != nil {
		t.Fatal(err)
	}
	revision := cached.Revision
	if !cached.HasLocalizationGeometry || cached.LocalizationGeometry == nil || len(cached.LocalizationGeometry.Architecture) == 0 {
		t.Fatal("geometry was not persisted")
	}
	if err := store.SaveLocalizationGeometry(input); err != nil {
		t.Fatal(err)
	}
	again, _ := store.Frame(frame)
	if again.Revision != revision {
		t.Fatal("unchanged geometry churned room revision")
	}
	restarted, _ := NewStore(directory)
	again, err = restarted.Frame(frame)
	if err != nil || again.LocalizationGeometry == nil {
		t.Fatalf("restart: %+v %v", again, err)
	}
	listed, _ := restarted.List()
	if len(listed) != 1 || !listed[0].HasLocalizationGeometry || listed[0].LocalizationGeometry != nil {
		t.Fatal("list should expose only availability")
	}
	// Room edits cannot replace the server's frame-bound mesh.
	again.LocalizationGeometry = &RoomLocalizationGeometry{Frame: "invented"}
	again.HasLocalizationGeometry = false
	updated, err := store.Put(again)
	if err != nil || !updated.HasLocalizationGeometry || updated.LocalizationGeometry.Frame != frame {
		t.Fatalf("untrusted overwrite: %+v %v", updated, err)
	}
	input.Frame = saved.Site.ID + "/" + uuid.NewString()
	if err := store.SaveLocalizationGeometry(input); err == nil {
		t.Fatal("accepted changed frame")
	}
	input = localizationGeometryFixture(frame)
	input.Architecture[0] = math.NaN()
	if err := store.SaveLocalizationGeometry(input); err == nil {
		t.Fatal("accepted nonfinite mesh")
	}
	input = localizationGeometryFixture(frame)
	input.Architecture = append(input.Architecture, 0)
	if err := store.SaveLocalizationGeometry(input); err == nil {
		t.Fatal("accepted malformed triangles")
	}
}

func TestLocalizationGeometryUploadAndPublicRoomResponses(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	saved, _ := store.Put(room())
	s := &Server{Store: store}
	handler := s.Handler()
	frame := saved.Site.ID + "/" + saved.Site.ModelRevision
	raw, _ := json.Marshal(localizationGeometryFixture(frame))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/v1/rooms/"+saved.Site.ID+"/localization-geometry", bytes.NewReader(raw)))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, path := range []string{"/v1/rooms", "/v1/rooms/" + saved.Site.ID + "/state", "/v1/rooms/" + saved.Site.ID} {
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"hasLocalizationGeometry":true`) || strings.Contains(w.Body.String(), `"architecture"`) {
			t.Fatalf("%s: %s", path, w.Body.String())
		}
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/v1/rooms/"+uuid.NewString()+"/localization-geometry", bytes.NewReader(raw)))
	if w.Code != 400 {
		t.Fatal("accepted geometry for another room")
	}
}
