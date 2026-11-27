package twin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

func benchmarkDenseRoom(b *testing.B) (*Store, Room) {
	b.Helper()
	store, err := NewStore(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	saved, err := store.Put(room())
	if err != nil {
		b.Fatal(err)
	}
	saved.Points = make([]Point, 20000)
	for i := range saved.Points {
		saved.Points[i] = Point{X: float64(i%200) * .05, Y: float64(i%17) * .1, Z: float64(i/200) * .05}
	}
	saved.PointLayers = map[string][]Point{"lidar": saved.Points}
	saved.References = map[string][]sitenav.Surface{"robot": make([]sitenav.Surface, 3000)}
	saved.HasLocalizationGeometry = true
	saved.LocalizationGeometry = &RoomLocalizationGeometry{Frame: saved.Site.ID + "/" + saved.Site.ModelRevision, Architecture: make([]float64, 45000)}
	for i := range saved.LocalizationGeometry.Architecture {
		saved.LocalizationGeometry.Architecture[i] = float64(i%113) * .07
	}
	if err := store.write(saved); err != nil {
		b.Fatal(err)
	}
	return store, saved
}
func BenchmarkDenseRoomStore(b *testing.B) {
	for _, operation := range []string{"list", "state", "frame", "withFrame"} {
		b.Run(operation, func(b *testing.B) {
			store, room := benchmarkDenseRoom(b)
			frame := room.Site.ID + "/" + room.Site.ModelRevision
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var err error
				switch operation {
				case "list":
					_, err = store.List()
				case "state":
					_, err = store.State(room.Site.ID)
				case "frame":
					_, err = store.Frame(frame)
				case "withFrame":
					err = store.withFrame(frame, func() error { return nil })
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type discardRoomResponse struct{ header http.Header }

func (w discardRoomResponse) Header() http.Header         { return w.header }
func (w discardRoomResponse) Write(p []byte) (int, error) { return io.Discard.Write(p) }
func (w discardRoomResponse) WriteHeader(int)             {}
func BenchmarkDenseRoomResponseGzip(b *testing.B) {
	_, room := benchmarkDenseRoom(b)
	req := httptest.NewRequest(http.MethodGet, "/v1/rooms/id/state", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		respondRoom(discardRoomResponse{header: make(http.Header)}, req, room, nil)
	}
}

func BenchmarkDenseRoomStateHTTP(b *testing.B) {
	store, room := benchmarkDenseRoom(b)
	handler := (&Server{Store: store}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/v1/rooms/"+room.Site.ID+"/state", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		handler.ServeHTTP(discardRoomResponse{header: make(http.Header)}, req)
	}
}

func BenchmarkDenseRoomFrameCold(b *testing.B) {
	store, room := benchmarkDenseRoom(b)
	frame := room.Site.ID + "/" + room.Site.ModelRevision
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.mu.Lock()
		store.forgetRoom(room.Site.ID)
		store.mu.Unlock()
		if _, err := store.Frame(frame); err != nil {
			b.Fatal(err)
		}
	}
}
