package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

func thermalViewerFixture(seq uint64, value float64) []byte {
	pixels := make([]float64, 768)
	for i := range pixels {
		pixels[i] = 25
	}
	pixels[35] = value
	b, _ := json.Marshal(map[string]any{"status": "live", "age": 0.1, "frame": map[string]any{"width": 32, "height": 24, "pixels": pixels, "seq": seq, "number": 1, "timestamp_us": seq + 1}})
	return b
}
func TestMLXThermalValidation(t *testing.T) {
	b := thermalViewerFixture(1, 85)
	m, err := decodeMLXThermal(b)
	if err != nil || m.Celsius != 85 || m.X != 3 || m.Y != 1 {
		t.Fatalf("measurement %+v: %v", m, err)
	}
	for _, pair := range [][2]string{{`"age":0.1`, `"age":4`}, {`"status":"live"`, `"status":"offline"`}, {`"width":32`, `"width":24`}, {`"timestamp_us":2`, `"timestamp_us":0`}, {`85`, `1001`}} {
		if _, err := decodeMLXThermal([]byte(strings.Replace(string(b), pair[0], pair[1], 1))); err == nil {
			t.Fatalf("accepted %v", pair)
		}
	}
}

func TestThermalCampaignRecordsLocallyAndLatches(t *testing.T) {
	var seq atomic.Uint64
	var hot atomic.Bool
	hot.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := 25.0
		if hot.Load() {
			v = 85
		}
		_, _ = w.Write(thermalViewerFixture(seq.Add(1), v))
	}))
	defer server.Close()
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	service.addAdapter(&inferenceTestAdapter{sources: []data.Source{{ID: "v4l2:/dev/video0", Kind: "camera", Healthy: true}}})
	raw := fmt.Sprintf(`version: 1
name: thermal-test
sources:
  - camera: /dev/video0
thermal:
  mlx90642_url: %s
  above_celsius: 80
  clear_below_celsius: 75
  for: 1s
  event: overheating
capture:
  buffer: 10s
  drain: 0s
  after_trigger: 20s
  triggers:
    - event: overheating
upload:
  when: manual
export:
  annotation: none
`, server.URL)
	campaign, err := manager.DeployCampaign([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	stop := service.StartCampaignInference(context.Background(), nil, nil)
	defer stop()
	defer func() {
		for _, key := range manager.ActiveEpisodeKeys() {
			_, _ = service.stopCapture(context.Background(), key)
		}
	}()
	deadline := time.Now().Add(8 * time.Second)
	for {
		if _, ok := manager.ActiveSession(campaign.Name); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no thermal episode")
		}
		time.Sleep(20 * time.Millisecond)
	}
	episode, err := service.stopCapture(context.Background(), campaign.Name)
	if err != nil {
		t.Fatal(err)
	}
	manifest, warnings, err := manager.Inspect(episode.Id, true)
	if err != nil || len(warnings) > 0 || manifest.Trigger.CampaignName != campaign.Name {
		t.Fatalf("sealed episode: %+v %v %v", manifest, warnings, err)
	}
	f, _, err := manager.OpenFile(episode.Id, "events.jsonl", 0)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := io.ReadAll(f)
	f.Close()
	if err != nil || !strings.Contains(string(evidence), `"max_celsius":85`) || !strings.Contains(string(evidence), `"provenance":"MLX90642`) {
		t.Fatalf("missing thermal evidence: %s %v", evidence, err)
	}
	time.Sleep(2 * time.Second)
	if _, ok := manager.ActiveSession(campaign.Name); ok {
		t.Fatal("sustained overheating started a duplicate episode")
	}
	hot.Store(false)
	time.Sleep(time.Second)
	hot.Store(true)
	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, ok := manager.ActiveSession(campaign.Name); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("did not rearm after cooling")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
