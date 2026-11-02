package btradio

import (
	"context"
	"errors"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"sync/atomic"
	"testing"
)

func TestUnsubscribeCannotRemoveReplacement(t *testing.T) {
	resetPolicy(t)
	old, err := Register(context.Background(), "mesh", func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	current, err := Register(context.Background(), "mesh", func(context.Context) error { calls++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer current()
	old()
	if err := Set(context.Background(), &pb.BluetoothPowerSetting{Mode: "maximum"}); err != nil || calls != 2 {
		t.Fatal(calls, err)
	}
}
func TestStoppedOwnerCannotBeReapplied(t *testing.T) {
	resetPolicy(t)
	owner, cancel := context.WithCancel(context.Background())
	calls := 0
	stop, err := Register(owner, "mesh", func(context.Context) error { calls++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	cancel()
	if err := Set(context.Background(), &pb.BluetoothPowerSetting{Mode: "maximum"}); err != nil || calls != 1 {
		t.Fatal(calls, err)
	}
}
func TestOwnerCancellationInterruptsInFlightApply(t *testing.T) {
	resetPolicy(t)
	owner, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	var calls atomic.Int32
	stop, err := Register(owner, "mesh", func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			return nil
		}
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	done := make(chan error, 1)
	go func() { done <- Set(context.Background(), &pb.BluetoothPowerSetting{Mode: "maximum"}) }()
	<-entered
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestInitialApplyFailureCanRecoverThroughManagement(t *testing.T) {
	resetPolicy(t)
	fail := true
	stop, err := Register(context.Background(), "management", func(context.Context) error {
		if fail {
			return errors.New("unsupported saved policy")
		}
		return nil
	})
	if err == nil || stop == nil {
		t.Fatal("failed registration lost recovery subscription")
	}
	defer stop()
	fail = false
	if err := Set(context.Background(), &pb.BluetoothPowerSetting{Mode: "default"}); err != nil {
		t.Fatal(err)
	}
}
func TestAdvertisementWaitIsCancellable(t *testing.T) {
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- WithAdvertisement(context.Background(), func() error { close(entered); <-release; return nil })
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WithAdvertisement(ctx, func() error { t.Error("cancelled refresh executed"); return nil })
	close(release)
	if e := <-finished; e != nil {
		t.Fatal(e)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestOptionalIntervalsNeverDiscardExplicitPower(t *testing.T) {
	original := map[string]int{"TxPower": 8, "MinInterval": 160, "MaxInterval": 320, "ServiceData": 42}
	calls := 0
	err := registerAdvertisement(original, func(properties map[string]int) error {
		if properties["TxPower"] != 8 || properties["ServiceData"] != 42 {
			t.Fatal(properties)
		}
		if calls == 1 {
			if _, ok := properties["MinInterval"]; ok {
				t.Fatal("interval retained on retry")
			}
		}
		return nil
	}, func() error {
		calls++
		if calls == 1 {
			return errors.New("intervals unsupported")
		}
		return nil
	})
	if err != nil || calls != 2 || original["MinInterval"] != 160 {
		t.Fatal(err, calls, original)
	}
	calls = 0
	err = registerAdvertisement(map[string]int{"TxPower": 8}, func(map[string]int) error { return nil }, func() error { calls++; return errors.New("power unsupported") })
	if err == nil || calls != 1 {
		t.Fatal("explicit power silently retried", err, calls)
	}
}

func TestPowerCapabilityRanges(t *testing.T) {
	resetPolicy(t)
	if err := Set(context.Background(), &pb.BluetoothPowerSetting{Mode: "maximum"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		min, max int32
		want     int16
		valid    bool
	}{{-40, 8, 8, true}, {-127, 0, 0, true}, {-40, 30, 20, true}, {9, 8, 0, false}, {-128, 8, 0, false}, {21, 30, 0, false}} {
		p, err := Request(true, tc.min, tc.max)
		if tc.valid {
			if err != nil || p == nil || *p != tc.want {
				t.Fatal(tc, p, err)
			}
		} else if err == nil {
			t.Fatal("invalid range accepted", tc, p)
		}
	}
}

func TestQueuedAdvertisementReadsPayloadAfterPriorEffect(t *testing.T) {
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var payload atomic.Int32
	go func() {
		finished <- WithAdvertisement(context.Background(), func() error { close(entered); <-release; payload.Store(2); return nil })
	}()
	<-entered
	result := make(chan error, 1)
	go func() {
		result <- WithAdvertisementSnapshot(context.Background(), func() (int32, error) { return payload.Load(), nil }, func(value int32) error {
			if value != 2 {
				return errors.New("stale advertisement payload")
			}
			return nil
		})
	}()
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestPreflightFailureRetainsLastKnownRegistration(t *testing.T) {
	resetPolicy(t)
	power := int16(8)
	Report("mesh", &power, nil)
	ReportFailure("mesh", context.DeadlineExceeded)
	got := Reports()
	if len(got) != 1 || !got[0].Registered || got[0].GetRequestedDbm() != 8 || got[0].Error == "" {
		t.Fatal(got)
	}
	ReportFailure("management", errors.New("initial request unsupported"))
	got = Reports()
	if got[0].Name != "management" || got[0].Registered || got[0].Error == "" {
		t.Fatal(got)
	}
}
