package stationarycamera

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestEmulatedCameraRunPersistsEveryReceiptAncestry(t *testing.T) {
	cfg := Config{Dir: t.TempDir(), Tenant: "test", Site: "simulator", Device: "emulated-xiao", Camera: "0", SimulationRun: "firmware-run-42"}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	data := testJPEG(t)
	if err := s.AcceptJPEG(data, "wrong-origin", 1, world.Measured); err == nil {
		t.Fatal("configured emulation accepted measured data")
	}
	for sequence := uint64(1); sequence <= 3; sequence++ {
		if err := s.AcceptJPEG(data, "firmware-boot-1", sequence, world.Simulated); err != nil {
			t.Fatal(err)
		}
		assertCameraRunAncestry(t, s, cfg.SimulationRun, int(sequence))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"", "different-run"} {
		changed := cfg
		changed.SimulationRun = runID
		if reopened, err := Open(changed); err == nil {
			reopened.Close()
			t.Fatalf("reopened simulated history with run %q", runID)
		}
	}
	s, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.State().Fresh {
		t.Fatal("restart falsely made old simulation frames fresh")
	}
	assertCameraRunAncestry(t, s, cfg.SimulationRun, 3)
	if err := s.AcceptJPEG(data, "firmware-boot-1", 4, world.Simulated); err == nil {
		t.Fatal("restart revived retired firmware stream")
	}
	if err := s.AcceptJPEG(data, "firmware-boot-2", 1, world.Measured); err == nil {
		t.Fatal("restart allowed simulated origin to become measured")
	}
	if err := s.AcceptJPEG(data, "firmware-boot-2", 1, world.Simulated); err != nil {
		t.Fatal(err)
	}
	assertCameraRunAncestry(t, s, cfg.SimulationRun, 4)
}

func assertCameraRunAncestry(t *testing.T, s *Store, runID string, count int) {
	t.Helper()
	state := s.State()
	if len(state.Recent) != count {
		t.Fatalf("got %d receipts, want %d", len(state.Recent), count)
	}
	for _, receipt := range state.Recent {
		if receipt.Origin != world.Simulated || receipt.SimulationRun != runID || receipt.ExposureTimeKnown {
			t.Fatalf("receipt lost simulation ancestry or invented exposure: %+v", receipt)
		}
	}
	records, err := s.world.Records()
	if err != nil || len(records) != 1 {
		t.Fatalf("world records: %v, count %d", err, len(records))
	}
	p := records[0].Provenance
	if p.Origin != world.Simulated || !p.Synthetic() || p.Simulation == nil || p.Simulation.RunID != runID || p.Simulation.ScenarioVersion == "" || p.Simulation.ClockDomain != records[0].Timing.ClockDomain {
		t.Fatalf("world component lost simulation context: %+v", p)
	}
}

func TestPhysicalCameraCannotReopenAsEmulatedRun(t *testing.T) {
	s, _, cfg := fixture(t)
	if err := s.AcceptJPEG(testJPEG(t), "physical-stream", 1, world.Measured); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.SimulationRun = "firmware-run"
	if reopened, err := Open(cfg); err == nil {
		reopened.Close()
		t.Fatal("physical history reopened as simulated camera")
	}
}
