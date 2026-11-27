package replication

import (
	"context"
	"errors"
	"testing"
)

func TestBoundedPreAndPostCommitChecks(t *testing.T) {
	for _, mode := range []string{"two-successes", "postcommit-invalidation", "sticky-first-failure", "missing", "late"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t, recordItem(t, "gas", 1))
			calls := 0
			f.b.cfg.RemoteAuthority = func(context.Context, Item) error { calls++; return nil }
			var late func(context.Context) error
			var second error
			f.local.admit = func(ctx context.Context, item Item, check func(context.Context) error) error {
				switch mode {
				case "missing":
					return nil
				case "late":
					late = check
					return nil
				case "sticky-first-failure":
					f.generation.Store(2)
					if e := check(ctx); e == nil {
						t.Fatal("first check accepted invalid policy")
					}
					f.generation.Store(1)
					second = check(ctx)
					return nil
				default:
					if e := check(ctx); e != nil {
						return e
					}
					// Model a durable commit before the second authority boundary.
					f.local.items[item.Entry.OrderKey()] = cloneItem(item)
					if mode == "postcommit-invalidation" {
						f.generation.Store(2)
					}
					second = check(ctx)
					return second
				}
			}
			r, e := f.b.SyncPeer(context.Background(), deviceA)
			if mode == "two-successes" {
				if e != nil || !r.Complete || r.Admitted != 1 || r.UnknownOutcome || calls != 3 {
					t.Fatal(r, e, calls)
				}
			} else {
				if !errors.Is(e, ErrUncertain) || !r.UnknownOutcome || r.Complete || r.Admitted != 0 {
					t.Fatal(r, e)
				}
				if (mode == "postcommit-invalidation" || mode == "sticky-first-failure") && second == nil {
					t.Fatal("failed check was hidden")
				}
			}
			if mode == "postcommit-invalidation" && len(f.local.items) != 1 {
				t.Fatal("test did not cross persistence boundary")
			}
			if mode == "sticky-first-failure" && calls != 2 {
				t.Fatal("latched failure performed another authority check", calls)
			}
			if late != nil {
				before := calls
				if err := late(context.Background()); err == nil || calls != before {
					t.Fatal("late check reached authority or was accepted", err)
				}
			}
		})
	}
}

func TestRetainedCheckCannotGrantAuthorityAfterSuccessfulReturn(t *testing.T) {
	f := setup(t, recordItem(t, "gas", 1))
	calls := 0
	f.b.cfg.RemoteAuthority = func(context.Context, Item) error { calls++; return nil }
	var retained func(context.Context) error
	f.local.admit = func(ctx context.Context, _ Item, check func(context.Context) error) error {
		retained = check
		return check(ctx)
	}
	r, e := f.b.SyncPeer(context.Background(), deviceA)
	if e != nil || !r.Complete {
		t.Fatal(r, e)
	}
	before := calls
	for range 8 {
		if err := retained(context.Background()); err == nil {
			t.Fatal("retained callback accepted")
		}
	}
	if calls != before {
		t.Fatal("late callback performed authority work")
	}
}
