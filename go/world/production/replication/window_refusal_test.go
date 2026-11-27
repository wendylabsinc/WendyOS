package replication

import (
	"context"
	"errors"
	"testing"
)

func TestRecordWindowDeferredStopsOnlyCurrentRound(t *testing.T) {
	f := setup(t, recordItem(t, "gas", 1))
	calls := 0
	f.local.admit = func(ctx context.Context, item Item, check func(context.Context) error) error {
		calls++
		if calls == 1 {
			return ErrRecordWindowDeferred
		}
		if err := check(ctx); err != nil {
			return err
		}
		f.local.items[item.Entry.OrderKey()] = cloneItem(item)
		return check(ctx)
	}
	r, err := f.b.SyncPeer(context.Background(), deviceA)
	if !errors.Is(err, ErrRecordWindowDeferred) || errors.Is(err, ErrUncertain) || !r.Deferred || r.UnknownOutcome || r.Complete || r.Admitted != 0 || calls != 1 {
		t.Fatal("window refusal was retried or made uncertain", r, err, calls)
	}
	r, err = f.b.SyncPeer(context.Background(), deviceA)
	if err != nil || !r.Complete || r.UnknownOutcome || r.Deferred || r.Admitted != 1 || calls != 2 {
		t.Fatal(r, err, calls)
	}
}

func TestRecordWindowDeferredNeverAppliesToLeaseOrFloor(t *testing.T) {
	for _, item := range []Item{leaseItem(t), floorItem(t, 9)} {
		f := setup(t, item)
		f.local.admit = func(context.Context, Item, func(context.Context) error) error { return ErrRecordWindowDeferred }
		r, err := f.b.SyncPeer(context.Background(), deviceA)
		if !errors.Is(err, ErrUncertain) || !r.UnknownOutcome || r.Deferred {
			t.Fatal(item.Entry.Kind, r, err)
		}
	}
}

func TestRecordWindowDeferredCannotMaskUncertainAdmissionChecks(t *testing.T) {
	for _, mode := range []string{"unmarked", "marked-uncertain", "failed-check", "two-checks", "excess-check", "canceled", "missing-check", "post-return"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t, recordItem(t, "gas", 1))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.local.admit = func(ctx context.Context, _ Item, check func(context.Context) error) error {
				switch mode {
				case "unmarked":
					return errors.New("unclassified admission failure")
				case "marked-uncertain":
					return errors.Join(ErrRecordWindowDeferred, ErrUncertain)
				case "failed-check":
					f.generation.Store(2)
					_ = check(ctx)
				case "two-checks":
					_ = check(ctx)
					_ = check(ctx)
				case "excess-check":
					for range MaxFinalChecks + 1 {
						_ = check(ctx)
					}
				case "canceled":
					cancel()
				case "missing-check":
					return nil
				case "post-return":
					if err := check(ctx); err != nil {
						t.Fatal(err)
					}
					f.generation.Store(2)
					return nil
				}
				return ErrRecordWindowDeferred
			}
			r, err := f.b.SyncPeer(ctx, deviceA)
			if !errors.Is(err, ErrUncertain) || !r.UnknownOutcome || r.Deferred || r.Complete {
				t.Fatal(r, err)
			}
		})
	}
}
