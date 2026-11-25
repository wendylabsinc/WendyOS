package servicejournal

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	bolt "go.etcd.io/bbolt"
)

func remoteDatabaseBytes(t *testing.T, f *remoteEnvironment) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := f.r.base.db.View(func(tx *bolt.Tx) error { _, err := tx.WriteTo(&out); return err }); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestRemoteRecordWindowRefusalDefersUntilOriginalNotBefore(t *testing.T) {
	f := remoteSetup(t)
	receiver := &clock{w: f.clock.w}
	verifier, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: f.scope.Tenant, Site: f.scope.Site,
		ExecutionDomain: "simulation", Certificates: f.publisherConfig.Certificates, Clock: receiver,
		Authorize: func(context.Context, serviceorigin.DelegationClaims, wendycert.DeviceIdentity) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	f.r.base.cfg.Verifier, f.r.base.cfg.PhysicalClock = verifier, receiver
	f.clock.advance(500 * time.Millisecond)
	raw := f.raw(t)
	before := remoteDatabaseBytes(t, f)
	commits := 0
	originalCommit := f.r.base.commit
	f.r.base.commit = func(fn func(*bolt.Tx) error) error { commits++; return originalCommit(fn) }
	got, err := f.r.AdmitRemote(context.Background(), raw, f.cap(), accept)
	if got != "" || !IsRecordWindowRefusal(err) || !errors.Is(err, serviceorigin.ErrExpired) || commits != 0 {
		t.Fatal("future original was not a precommit refusal", got, err, commits)
	}
	if !bytes.Equal(before, remoteDatabaseBytes(t, f)) || len(f.r.base.proofs) != 0 || f.r.base.faulted || f.r.faulted.Load() {
		t.Fatal("refusal changed record storage or faulted journal")
	}
	receiver.advance(500 * time.Millisecond)
	got, err = f.r.AdmitRemote(context.Background(), raw, f.cap(), accept)
	if err != nil || got != world.Applied || commits != 1 {
		t.Fatal("same original did not become admissible", got, err, commits)
	}
	for _, stored := range f.r.base.proofs {
		if !bytes.Equal(stored.Envelope, raw) {
			t.Fatal("original was changed")
		}
	}
}

func TestRemoteRecordWindowRefusalTracksActualTransactionBoundary(t *testing.T) {
	for _, mode := range []string{"transaction-verify", "precommit-window", "callback-error", "postcommit-window", "commit-response", "clock-fault", "already-faulted"} {
		t.Run(mode, func(t *testing.T) {
			f := remoteSetup(t)
			raw := f.raw(t)
			before := remoteDatabaseBytes(t, f)
			calls := 0
			check := func(context.Context) error {
				calls++
				switch mode {
				case "precommit-window":
					f.clock.advance(2 * time.Second)
				case "callback-error":
					return serviceorigin.ErrExpired
				case "postcommit-window":
					if calls == 2 {
						f.clock.advance(11 * time.Minute)
					}
				case "clock-fault":
					f.clock.advance(-time.Nanosecond)
				}
				return nil
			}
			switch mode {
			case "transaction-verify":
				f.r.base.commit = func(fn func(*bolt.Tx) error) error {
					f.clock.advance(11 * time.Minute)
					return f.r.base.db.Update(fn)
				}
			case "precommit-window":
				f.authorityUntil = f.clock.w.Latest.Add(time.Second)
			case "commit-response":
				f.r.base.commit = func(fn func(*bolt.Tx) error) error {
					if err := f.r.base.db.Update(fn); err != nil {
						return err
					}
					return serviceorigin.ErrExpired // A commit response failure must not be reclassified.
				}
			case "already-faulted":
				f.r.base.faulted = true
			}
			_, err := f.r.AdmitRemote(context.Background(), raw, f.cap(), check)
			deferred := mode == "transaction-verify" || mode == "precommit-window"
			committed := mode == "commit-response" || mode == "postcommit-window"
			if err == nil || IsRecordWindowRefusal(err) != deferred {
				t.Fatal("wrong admission classification", err)
			}
			if committed && !errors.Is(err, ErrFaulted) {
				t.Fatal("commit uncertainty was lost", err)
			}
			if !committed && !bytes.Equal(before, remoteDatabaseBytes(t, f)) {
				t.Fatal("rejected transaction wrote record state")
			}
			// Reopen inspects the actual durable record, including a lost commit response.
			f.authorityUntil = f.clock.w.Latest.Add(time.Hour)
			if mode == "clock-fault" {
				return
			}
			f.reopenRemote(t)
			if (len(f.r.base.proofs) == 1) != committed {
				t.Fatal("durable commit boundary differs")
			}
		})
	}
}

func TestRemoteRecordWindowRefusalCannotHideFaultOrCancellation(t *testing.T) {
	refused := &recordWindowRefusal{cause: serviceorigin.ErrExpired}
	for _, terminal := range []error{ErrFaulted, context.Canceled, context.DeadlineExceeded} {
		if IsRecordWindowRefusal(errors.Join(refused, terminal)) {
			t.Fatal("terminal outcome hidden", terminal)
		}
	}
	if IsRecordWindowRefusal(serviceorigin.ErrExpired) {
		t.Fatal("unmarked error became deferrable")
	}
}
