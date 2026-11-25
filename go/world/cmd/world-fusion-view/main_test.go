package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusionview"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestProtocolRejectsMalformedBatchThenContinues(t *testing.T) {
	cfg := fusionview.Config{Tenant: "simulation", Site: "studio", RunID: "run", ClockDomain: "simulation:run", Frame: world.FrameRef{ID: "floor", Revision: "1"}, Gateways: map[string]world.Source{"hub": {Device: "hub", App: "fusion", Service: "hazard", Stream: "claim"}}, Hold: time.Second, Improvement: .2}
	input := "{\"now\":\"2026-01-01T00:00:00Z\",\"reports\":[],\"unexpected\":true}\n{\"now\":\"2026-01-01T00:00:00Z\",\"reports\":[]}\n"
	var output bytes.Buffer
	if err := serve(strings.NewReader(input), &output, cfg); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	var invalid map[string]any
	decoder.Decode(&invalid)
	if invalid["error"] == nil {
		t.Fatal("unknown field accepted")
	}
	var view fusionview.View
	if err := decoder.Decode(&view); err != nil || view.State != "unavailable" {
		t.Fatalf("valid subsequent request failed: %+v %v", view, err)
	}
}
func TestSingleObjectRequired(t *testing.T) {
	var req request
	if decode([]byte(`{} {}`), &req) == nil {
		t.Fatal("trailing object accepted")
	}
}
