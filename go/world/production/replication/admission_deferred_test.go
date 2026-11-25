package replication

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestAdmissionNotStartedDefersLatestAndRepairWithoutRetry(t *testing.T) {
	for _, latest := range []bool{false, true} {
		for _, item := range []Item{recordItem(t, "gas", 1), leaseItem(t), floorItem(t, 9)} {
			t.Run(fmt.Sprintf("latest=%t/kind=%d", latest, item.Entry.Kind), func(t *testing.T) {
				f := setup(t, item)
				calls := 0
				f.local.admit = func(ctx context.Context, got Item, check func(context.Context) error) error {
					calls++
					if calls == 1 {
						return AdmissionNotStarted(ErrDenied)
					}
					if err := check(ctx); err != nil {
						return err
					}
					f.local.items[got.Entry.OrderKey()] = cloneItem(got)
					return check(ctx)
				}
				run := func() (Result, error) {
					if latest {
						conn, _ := latestConnection(f)
						return f.a.SendLatest(context.Background(), conn, []Item{item})
					}
					return f.b.SyncPeer(context.Background(), deviceA)
				}
				r, err := run()
				if err == nil || !r.Deferred || r.UnknownOutcome || r.Complete || r.Admitted != 0 || calls != 1 || len(f.local.items) != 0 {
					t.Fatal(r, err, calls)
				}
				if latest && (!errors.Is(err, ErrAdmissionDeferred) || errors.Is(err, ErrRecordWindowDeferred)) {
					t.Fatal("wire invented specific refusal reason", err)
				}
				if !latest && !IsAdmissionNotStarted(err) {
					t.Fatal("local reason lost", err)
				}
				r, err = run()
				got := f.local.items[item.Entry.OrderKey()]
				if err != nil || !r.Complete || r.UnknownOutcome || r.Admitted != 1 || calls != 2 || got.Entry != item.Entry || !bytes.Equal(got.Envelope, item.Envelope) {
					t.Fatal(r, err, calls)
				}
			})
		}
	}
}

func TestAdmissionNotStartedCannotHideChecksOrOtherErrors(t *testing.T) {
	for _, mode := range []string{"unmarked", "one-check", "failed-check", "two-checks", "joined", "wrapped", "uncertain-cause", "nil-cause"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t, recordItem(t, "gas", 1))
			f.local.admit = func(ctx context.Context, _ Item, check func(context.Context) error) error {
				refusal := AdmissionNotStarted(ErrDenied)
				switch mode {
				case "unmarked":
					return ErrDenied
				case "one-check":
					_ = check(ctx)
				case "failed-check":
					f.generation.Store(2)
					_ = check(ctx)
				case "two-checks":
					_ = check(ctx)
					_ = check(ctx)
				case "joined":
					return errors.Join(refusal, ErrUncertain)
				case "wrapped":
					return fmt.Errorf("different boundary: %w", refusal)
				case "uncertain-cause":
					return AdmissionNotStarted(ErrUncertain)
				case "nil-cause":
					return AdmissionNotStarted(nil)
				}
				return refusal
			}
			conn, _ := latestConnection(f)
			r, err := f.a.SendLatest(context.Background(), conn, []Item{recordItem(t, "gas", 1)})
			if !errors.Is(err, ErrUncertain) || !r.UnknownOutcome || r.Deferred || r.Complete {
				t.Fatal(r, err)
			}
		})
	}
}

func TestAdmissionNotStartedCancellationIsKnownNoAdmission(t *testing.T) {
	f := setup(t, recordItem(t, "gas", 1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.local.admit = func(context.Context, Item, func(context.Context) error) error {
		cancel()
		return AdmissionNotStarted(context.Canceled)
	}
	r, err := f.b.SyncPeer(ctx, deviceA)
	if !IsAdmissionNotStarted(err) || !errors.Is(err, context.Canceled) || !r.Deferred || r.UnknownOutcome || r.Complete {
		t.Fatal(r, err)
	}
}
