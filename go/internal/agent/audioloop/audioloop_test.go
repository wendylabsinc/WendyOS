package audioloop

import (
	"context"
	"errors"
	"testing"
)

func TestAllocateStableAndUnique(t *testing.T) {
	m := NewManager(nil)
	a, err := m.Allocate(1, 10, "mic0")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := m.Allocate(1, 10, "mic0"); again != a {
		t.Fatalf("expected stable subdevice, got %d then %d", a, again)
	}
	b, _ := m.Allocate(2, 10, "mic0")
	if b == a {
		t.Fatalf("different (source,channel) must get distinct subdevices, both %d", a)
	}
}

func TestAllocateExhaustion(t *testing.T) {
	m := NewManager(nil)
	for i := 0; i < MaxSubdevices; i++ {
		if _, err := m.Allocate(int32(i), 0, "mic0"); err != nil {
			t.Fatalf("alloc %d: %v", i, err)
		}
	}
	if _, err := m.Allocate(99, 0, "mic0"); err == nil {
		t.Fatal("expected exhaustion error")
	}
}

// TestMountsReportsAllocations asserts Mounts returns, per allocated subdevice,
// the (source asset id, channel, sensor name) it was allocated for — the
// mapping the audio device enumeration names Loopback capture subdevices from.
func TestMountsReportsAllocations(t *testing.T) {
	m := NewManager(nil)
	subA, _ := m.Allocate(286, 1, "mic0")
	subB, _ := m.Allocate(287, 2, "mic1")

	mounts := m.Mounts()
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d: %+v", len(mounts), mounts)
	}
	// Sorted by subdevice index.
	by := map[int]Mount{}
	for _, mt := range mounts {
		by[mt.Sub] = mt
	}
	if got := by[subA]; got.SourceAssetID != 286 || got.ChannelID != 1 || got.SensorName != "mic0" {
		t.Fatalf("mount for sub %d = %+v, want asset 286 chan 1 mic0", subA, got)
	}
	if got := by[subB]; got.SourceAssetID != 287 || got.ChannelID != 2 || got.SensorName != "mic1" {
		t.Fatalf("mount for sub %d = %+v, want asset 287 chan 2 mic1", subB, got)
	}
}

func TestAplayArgs(t *testing.T) {
	got := aplayArgs("hw:Loopback,0,3", PCMFormat{SampleRate: 48000, Channels: 2})
	want := []string{"-D", "hw:Loopback,0,3", "-f", "S16_LE", "-r", "48000", "-c", "2", "-t", "raw", "--buffer-time=200000", "--period-time=25000", "-q", "-"}
	if len(got) != len(want) {
		t.Fatalf("args %v != %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d: %q != %q", i, got[i], want[i])
		}
	}
}

func TestEnsureModuleRetriesFailureAndCachesSuccess(t *testing.T) {
	m := NewManager(nil)
	attempts := 0
	m.deps.modprobe = func(context.Context) error {
		attempts++
		if attempts == 1 {
			return errors.New("module temporarily unavailable")
		}
		return nil
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.EnsureModule(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled load: %v", err)
	}
	if attempts != 0 {
		t.Fatal("attempted load with cancelled context")
	}
	if err := m.EnsureModule(context.Background()); err == nil {
		t.Fatal("expected initial failure")
	}
	if err := m.EnsureModule(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureModule(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("got %d attempts, want 2", attempts)
	}
}

func TestUnpairedSourcesDoNotExhaustSlots(t *testing.T) {
	m := NewManager(nil)
	retained, _ := m.Allocate(100, 1, "retained")
	for source := int32(1); source <= MaxSubdevices*2; source++ {
		sub, err := m.Allocate(source, 1, "mic")
		if err != nil {
			t.Fatalf("source %d: %v", source, err)
		}
		if sub == retained {
			t.Fatal("reused another pairing's slot")
		}
		m.ReleaseSource(source)
		mounts := m.Mounts()
		if len(mounts) != 1 || mounts[0].SourceAssetID != 100 {
			t.Fatalf("stale mounts: %v", mounts)
		}
	}
}
