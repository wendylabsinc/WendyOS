package bluetooth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBLESetupRetriesWhenAdapterArrivesAfterStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts, failures := 0, 0
	maintainBLESetup(ctx, time.Millisecond, func(context.Context) bool { return true }, func(context.Context) error {
		attempts++
		if attempts == 1 {
			return errors.New("no Bluetooth adapter")
		}
		cancel()
		return nil
	}, func(error) { failures++ })
	if attempts != 2 || failures != 1 {
		t.Fatalf("attempts=%d failures=%d", attempts, failures)
	}
}

func TestBLESetupOnlyStartsWhileEnrollmentIsAvailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checks, starts := 0, 0
	maintainBLESetup(ctx, time.Millisecond, func(context.Context) bool {
		checks++
		if checks == 4 {
			cancel()
		}
		// The first check has no usable enrollment state. After the second
		// check the device is enrolled and must not advertise again.
		return checks == 2
	}, func(context.Context) error {
		starts++
		return nil
	}, func(err error) { t.Fatal(err) })
	if starts != 1 {
		t.Fatalf("setup started %d times", starts)
	}
}
