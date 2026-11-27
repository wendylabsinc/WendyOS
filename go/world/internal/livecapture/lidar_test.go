package livecapture

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

func addLidar(s *Snapshot, id string) {
	x := s.Observations[2]
	x.ID, x.SensorID, x.Kind = fmt.Sprintf("%s/%s/%d", s.RunID, id, s.Sequence), id, "lidar"
	x.Gas = nil
	x.PositionM = []float64{7, 1.2, 3}
	x.Lidar = &sensing.Lidar{PointsM: [][3]float64{{9, 1.2, 3}, {8, .3, 4}}, MaxRangeM: 6.1}
	s.Observations = append(s.Observations, x)
}

func TestCaptureOptionalLidarPreservesFrameAndTiming(t *testing.T) {
	now := time.Unix(1700000000, 0)
	s := fixture(now)
	for i := 0; i < MaxObservations-4; i++ {
		id := fmt.Sprintf("lidar-%d", i)
		addLidar(&s, id)
		s.State.Sensors[id] = true
	}
	got, err := Decode(encode(t, s), testConfig(&now))
	if err != nil || !reflect.DeepEqual(got, s) || len(got.Media) != 3 {
		t.Fatalf("optional inline scans changed original snapshot: %v", err)
	}
	for name, change := range map[string]func(*Snapshot){
		"duplicate":             func(s *Snapshot) { s.Observations[5] = s.Observations[4] },
		"wrong kind":            func(s *Snapshot) { s.Observations[4].Kind = "gas" },
		"unavailable":           func(s *Snapshot) { s.State.Sensors["lidar-0"] = false },
		"clock mismatch":        func(s *Snapshot) { s.Observations[4].SampleNS++ },
		"too many":              func(s *Snapshot) { addLidar(s, "lidar-extra") },
		"unknown sensor status": func(s *Snapshot) { s.State.Sensors["laser"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			copy, e := Decode(encode(t, s), testConfig(&now))
			if e != nil {
				t.Fatal(e)
			}
			change(&copy)
			if _, e = Decode(encode(t, copy), testConfig(&now)); e == nil {
				t.Fatal("accepted invalid optional scan")
			}
		})
	}
}

func TestClientLidarCannotRewriteSameCapture(t *testing.T) {
	now := time.Unix(1700000000, 0)
	s := fixture(now)
	addLidar(&s, "lidar-front")
	var body atomic.Value
	body.Store(encode(t, s))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body.Load().([]byte)) }))
	defer server.Close()
	cfg := testConfig(&now)
	cfg.URL = server.URL
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.Capture(context.Background())
	if err != nil || !reflect.DeepEqual(first.Observations[4], s.Observations[4]) {
		t.Fatal(err)
	}
	s.Observations[4].Lidar.PointsM[0][0]++
	body.Store(encode(t, s))
	if _, err = client.Capture(context.Background()); err == nil {
		t.Fatal("accepted changed point bytes at the same capture sequence")
	}
}
