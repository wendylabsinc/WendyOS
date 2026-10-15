//go:build linux

package nanprovider

import (
	"context"
	"errors"
	"testing"
	"time"
)

type publishFixture struct {
	replies []string
	errors  []error
	calls   int
}

func (f *publishFixture) command(_ string) (string, error) {
	i := f.calls
	f.calls++
	return f.replies[i], f.errors[i]
}

func TestPublishRetriesTransientFailInSameRadioEpoch(t *testing.T) {
	f := &publishFixture{
		replies: []string{"FAIL", "FAIL", "42"},
		errors:  []error{errors.New("supplicant NAN_PUBLISH: FAIL"), errors.New("supplicant NAN_PUBLISH: FAIL"), nil},
	}
	reply, err := publishWithRetry(context.Background(), f, "NAN_PUBLISH test", 0)
	if err != nil || reply != "42" || f.calls != 3 {
		t.Fatalf("publish reply=%q err=%v calls=%d", reply, err, f.calls)
	}
}

func TestPublishRetryIsBoundedAndOnlyForPlainFail(t *testing.T) {
	f := &publishFixture{replies: make([]string, 6), errors: make([]error, 6)}
	for i := range f.replies {
		f.replies[i] = "FAIL"
		f.errors[i] = errors.New("supplicant NAN_PUBLISH: FAIL")
	}
	if _, err := publishWithRetry(context.Background(), f, "NAN_PUBLISH test", 0); err == nil || f.calls != 6 {
		t.Fatalf("unbounded plain FAIL retry: err=%v calls=%d", err, f.calls)
	}
	f = &publishFixture{replies: []string{"FAIL-BUSY"}, errors: []error{errors.New("supplicant NAN_PUBLISH: FAIL-BUSY")}}
	if _, err := publishWithRetry(context.Background(), f, "NAN_PUBLISH test", 0); err == nil || f.calls != 1 {
		t.Fatalf("non-plain failure retried: err=%v calls=%d", err, f.calls)
	}
}

func TestPublishRetryStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &publishFixture{replies: []string{"FAIL"}, errors: []error{errors.New("supplicant NAN_PUBLISH: FAIL")}}
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	if _, err := publishWithRetry(ctx, f, "NAN_PUBLISH test", time.Hour); !errors.Is(err, context.Canceled) || f.calls != 1 {
		t.Fatalf("canceled retry err=%v calls=%d", err, f.calls)
	}
}
