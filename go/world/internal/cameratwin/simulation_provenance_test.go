package cameratwin

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/camerapose"
	"github.com/wendylabsinc/wendy/go/world/internal/stationarycamera"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestCameraRunSurvivesPoseAndPersonPublication(t *testing.T) {
	for _, runID := range []string{"esp32-firmware-run-42", ""} {
		t.Run("run="+runID, func(t *testing.T) {
			s, _, cameras, batch, _ := fixture(t)
			defer s.Close()
			camera := cameras["one"]
			first := *camera.State.Latest
			first.SimulationRun = runID
			second := first
			second.ID, second.Sequence = "r2", 2
			camera.State.Latest = &second
			camera.State.Recent = []stationarycamera.Receipt{second, first}
			for _, receipt := range []stationarycamera.Receipt{first, second} {
				camera.Pose.Evidence = append(camera.Pose.Evidence, camerapose.Evidence{ReceiptID: receipt.ID, SHA256: receipt.SHA256, SourceSession: receipt.SourceSession, Sequence: receipt.Sequence, ReceivedAt: receipt.ReceivedAt, Origin: receipt.Origin})
			}
			cameras["one"] = camera
			if err := s.PublishPose("one"); err != nil {
				t.Fatal(err)
			}
			if err := s.Publish(batch); err != nil {
				t.Fatal(err)
			}
			want := runID
			if want == "" {
				want = "camera-fixture"
			}
			records, err := s.replica.Records()
			if err != nil || len(records) != 2 {
				t.Fatalf("pose and person records: %d, %v", len(records), err)
			}
			for _, record := range records {
				if record.Provenance.Origin != world.Inferred || len(record.Provenance.Inputs) == 0 {
					t.Fatalf("lost inferred ancestry: %+v", record.Provenance)
				}
				for _, input := range record.Provenance.Inputs {
					if input.Origin != world.Simulated || input.Simulation == nil || input.Simulation.RunID != want || input.Simulation.ClockDomain == "" {
						t.Fatalf("publication changed camera run: %+v", input)
					}
				}
			}
			// A long-lived accepted pose can outlast the adapter's recent window.
			camera.State.Recent = []stationarycamera.Receipt{second}
			cameras["one"] = camera
			input := s.leaf("one", first.SourceSession+":"+first.ID, world.Simulated)
			if input.Simulation == nil || input.Simulation.RunID != want {
				t.Fatalf("aged pose evidence lost immutable camera run: %+v", input)
			}
		})
	}
}
