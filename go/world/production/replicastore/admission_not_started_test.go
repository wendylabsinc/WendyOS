package replicastore

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/servicejournal"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
)

func latestRecord(t *testing.T, f *fixture) replication.Item {
	t.Helper()
	page, err := f.a.store.Inventory(context.Background(), f.a.cfg.Export, "", 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range page.Entries {
		if entry.Kind == replication.Record {
			item, found, err := f.a.store.Fetch(context.Background(), entry)
			if err != nil || !found {
				t.Fatal(found, err)
			}
			return item
		}
	}
	t.Fatal("record unavailable")
	return replication.Item{}
}

func TestAdmissionGuardNotEnteredPreservesRealOriginalAndLaterAdmission(t *testing.T) {
	for _, cancellation := range []bool{false, true} {
		t.Run(map[bool]string{false: "host-refusal", true: "context-while-waiting"}[cancellation], func(t *testing.T) {
			f := newFixture(t)
			f.lease(t)
			if _, err := f.syncService(t).SyncPeer(context.Background(), deviceA); err != nil {
				t.Fatal(err)
			}
			raw := f.publish(t)
			item := latestRecord(t, f)
			original := f.b.store.cfg.Guard
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			guardCalls, checks := 0, 0
			f.b.store.cfg.Guard = func(context.Context, func() error) error {
				guardCalls++
				if cancellation {
					cancel()
					return ctx.Err()
				}
				return ErrDenied
			}
			err := f.b.store.Admit(ctx, item, func(context.Context) error { checks++; return nil })
			if !replication.IsAdmissionNotStarted(err) || errors.Is(err, replication.ErrUncertain) || guardCalls != 1 || checks != 0 || f.b.store.faulted.Load() {
				t.Fatal("unentered guard was not a definite refusal", err, guardCalls, checks)
			}
			f.b.store.cfg.Guard = original
			if err = f.b.store.Close(); err != nil {
				t.Fatal(err)
			}
			f.b.store, err = Open(f.b.cfg)
			if err != nil {
				t.Fatal(err)
			}
			live, err := f.b.store.Snapshot(context.Background())
			if err != nil || len(live) != 0 {
				t.Fatal("refusal persisted the record", len(live), err)
			}
			if err = f.b.store.Admit(context.Background(), item, func(context.Context) error { checks++; return nil }); err != nil {
				t.Fatal(err)
			}
			live, err = f.b.store.Snapshot(context.Background())
			if err != nil || len(live) != 1 || !bytes.Equal(live[0].Envelope, raw) || live[0].Descriptor.ExpiresAtUnixNS != item.Entry.ExpiresAtUnixNS || checks != 2 {
				t.Fatal("later admission changed original or deadline", live, err, checks)
			}
		})
	}
}

func TestAdmissionNotStartedExcludesEnteredFaultedAndBrokenGuards(t *testing.T) {
	for _, mode := range []string{"no-callback-no-error", "entered-before-final-check", "postcommit", "twice", "journal-fault", "authority-fault", "store-fault", "uncertain"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.lease(t)
			if _, err := f.syncService(t).SyncPeer(context.Background(), deviceA); err != nil {
				t.Fatal(err)
			}
			f.publish(t)
			item := latestRecord(t, f)
			original := f.b.store.cfg.Guard
			f.b.store.cfg.Guard = func(ctx context.Context, fn func() error) error {
				switch mode {
				case "no-callback-no-error":
					return nil
				case "entered-before-final-check":
					f.b.allowed.Store(false)
					return original(ctx, fn)
				case "postcommit":
					if err := original(ctx, fn); err != nil {
						return err
					}
					return ErrDenied
				case "twice":
					return original(ctx, func() error {
						if err := fn(); err != nil {
							return err
						}
						return fn()
					})
				case "journal-fault":
					return servicejournal.ErrFaulted
				case "authority-fault":
					return sourceauthority.ErrFaulted
				case "store-fault":
					return ErrFaulted
				default:
					return replication.ErrUncertain
				}
			}
			checks := 0
			err := f.b.store.Admit(context.Background(), item, func(context.Context) error { checks++; return nil })
			if err == nil || replication.IsAdmissionNotStarted(err) {
				t.Fatal("unproven refusal classified", mode, err, checks)
			}
			if mode == "entered-before-final-check" && checks != 0 {
				t.Fatal("expected zero final checks after callback entry", checks)
			}
			if (mode == "postcommit" || mode == "twice") && (!errors.Is(err, ErrFaulted) || checks != 2) {
				t.Fatal("committed failure did not stay faulted", err, checks)
			}
		})
	}
}
