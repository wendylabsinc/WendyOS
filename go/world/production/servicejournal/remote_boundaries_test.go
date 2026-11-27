package servicejournal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	bolt "go.etcd.io/bbolt"
)

func TestRemoteRejectsInvalidProofBeforeCommit(t *testing.T) {
	for _, mode := range []string{"signature", "duplicate-json", "own-origin", "schema", "future", "unknown-clock", "zero-cap", "oversized", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			f := remoteSetup(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "future" {
				f.request.Timing.SampleTime = f.sourceNow.Add(time.Second)
			}
			if mode == "unknown-clock" {
				f.request.Timing.ErrorKnown = false
			}
			raw, cap := f.raw(t), f.cap()
			switch mode {
			case "signature":
				s, _, _ := decodeRemoteProof(raw)
				s.Signature[0] ^= 1
				raw, _ = json.Marshal(s)
			case "duplicate-json":
				raw = append([]byte(`{"record":"",`), bytes.TrimSpace(raw)[1:]...)
			case "own-origin":
				f.r.cfg.LocalDevice = f.scope.Device
			case "schema":
				f.r.base.cfg.AdmitSchema = func(context.Context, serviceorigin.VerifiedRecord) error { return serviceorigin.ErrDenied }
			case "zero-cap":
				cap = 0
			case "oversized":
				raw = bytes.Repeat([]byte{' '}, maxEnvelopeBytes+1)
			case "canceled":
				cancel()
			}
			calls := 0
			result, err := f.r.AdmitRemote(ctx, raw, cap, func(context.Context) error { calls++; return nil })
			if err == nil || result != "" || calls != 0 || len(f.r.base.proofs) != 0 || len(f.r.base.epochs) != 0 {
				t.Fatal("invalid input reached commit", result, err, calls)
			}
		})
	}
}

func TestRemoteExpiryAndClockFloorsAreNotRenewed(t *testing.T) {
	t.Run("signed-age", func(t *testing.T) {
		f := remoteSetup(t)
		f.request.Timing.AgeHint = time.Second
		raw := f.raw(t)
		f.admitRemote(t, raw, world.Applied)
		v, err := f.r.Snapshot(context.Background())
		if err != nil || len(v) != 1 || v[0].Descriptor.ExpiresAtUnixNS != f.cap()-int64(time.Second) {
			t.Fatal("signed age became receipt-relative", err)
		}
		f.sourceNow = f.sourceNow.Add(1200 * time.Millisecond)
		f.reopenRemote(t)
		if v, err = f.r.Snapshot(context.Background()); err != nil || len(v) != 0 {
			t.Fatal("signed age renewed after restart", err)
		}
	})
	t.Run("signer-expiry-with-live-sample", func(t *testing.T) {
		f := remoteSetup(t)
		f.request.TTL = time.Hour
		f.admitRemote(t, f.raw(t), world.Applied)
		f.clock.advance(11 * time.Minute)
		f.reopenRemote(t)
		live, err := f.r.Snapshot(context.Background())
		if err != nil || len(live) != 0 {
			t.Fatal("reopened live sample bypassed original signer expiry", err)
		}
	})
	for _, mode := range []string{"physical", "source"} {
		t.Run(mode+"-rollback", func(t *testing.T) {
			f := remoteSetup(t)
			f.admitRemote(t, f.raw(t), world.Applied)
			if mode == "physical" {
				f.clock.advance(time.Second)
			} else {
				f.sourceNow = f.sourceNow.Add(time.Second)
			}
			if _, err := f.r.Snapshot(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.r.Close()
			if mode == "physical" {
				f.clock.advance(-time.Second)
			} else {
				f.sourceNow = f.sourceNow.Add(-time.Second)
			}
			if opened, err := OpenRemote(f.remoteCfg); err == nil {
				opened.Close()
				t.Fatal("durable clock floor was lost")
			}
		})
	}
}

func TestRemoteGuardFailureAndCallbackOwnership(t *testing.T) {
	for _, mode := range []string{"missing", "twice-hidden", "post-error", "post-cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := remoteSetup(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.r.cfg.Guard = func(ctx context.Context, fn func() error) error {
				if mode == "missing" {
					return nil
				}
				if err := fn(); err != nil {
					return err
				}
				switch mode {
				case "twice-hidden":
					_ = fn()
				case "post-error":
					return serviceorigin.ErrDenied
				case "post-cancel":
					cancel()
				}
				return nil
			}
			result, err := f.r.AdmitRemote(ctx, f.raw(t), f.cap(), accept)
			if err == nil || result != "" {
				t.Fatal("broken guard acknowledged", result, err)
			}
			if mode != "missing" && !errors.Is(err, ErrFaulted) {
				t.Fatal("guard failure after commit not uncertain", err)
			}
		})
	}
	t.Run("owned-authority-projection", func(t *testing.T) {
		f := remoteSetup(t)
		original := f.r.cfg.Authorize
		f.r.cfg.Authorize = func(ctx context.Context, v serviceorigin.VerifiedRecord, w trustedtime.Interval) (AuthorityWindow, error) {
			window, err := original(ctx, v, w)
			clear(v.Bytes)
			clear(v.Record.Publication.Payload)
			v.Record.Scope.Publish[0].Stream = "altered"
			v.Delegation.Scope.Publish[0].Stream = "altered"
			return window, err
		}
		raw := f.raw(t)
		f.admitRemote(t, raw, world.Applied)
		got, err := f.r.Snapshot(context.Background())
		if err != nil || len(got) != 1 || !bytes.Equal(got[0].Envelope, raw) {
			t.Fatal("callback mutated original", err)
		}
	})
}

func TestRemoteRestoreRefusesForgedCheckpointExtension(t *testing.T) {
	f := remoteSetup(t)
	f.admitRemote(t, f.raw(t), world.Applied)
	if err := f.r.base.db.Update(func(tx *bolt.Tx) error {
		var cp world.Checkpoint
		if err := decodeChecked(tx.Bucket(metaBucket).Get(checkpointKey), &cp, f.cfg.MaxProofBytes); err != nil {
			return err
		}
		// Move checkpoint time past the record. Replica restore alone allows an
		// expired high-water entry capped at checkpoint time. Remote restore must
		// also retain its stricter original absolute sample deadline.
		cp.CapturedAt = f.sourceNow.Add(10 * time.Second)
		cp.Entries[0].ExpiresAt = cp.CapturedAt
		return putChecked(tx.Bucket(metaBucket), checkpointKey, cp)
	}); err != nil {
		t.Fatal(err)
	}
	f.sourceNow = f.sourceNow.Add(11 * time.Second)
	f.r.Close()
	if j, err := OpenRemote(f.remoteCfg); err == nil {
		j.Close()
		t.Fatal("recomputed checksums extended original deadline")
	}
}
