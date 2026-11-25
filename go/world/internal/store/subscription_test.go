package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/subscription"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"os"
	"os/exec"
	"testing"
	"time"
)

func storePolicy() *subscription.Policy {
	return &subscription.Policy{Version: 1, Tenant: "tenant", Site: "office", Selection: selection.Selection{Types: []string{"pose"}}, RequiredTypes: []string{sitepub.Type}}
}
func TestSubscriptionSurvivesRestartAndRejectsConfigurationChanges(t *testing.T) {
	path, cfg, now, c := fixture(t)
	policy := storePolicy()
	s, err := OpenSubscribed(path, cfg, policy)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, s, c)
	policy.Selection.Types[0] = "caller-mutated"
	if !s.AcceptsKey(c.Key) {
		t.Fatal("caller mutated policy")
	}
	tomb := c
	tomb.Version.Sequence++
	tomb.Tombstone = true
	tomb.Payload = nil
	apply(t, s, tomb)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	for _, p := range []*subscription.Policy{nil, {Version: 1, Tenant: "tenant", Site: "office", Selection: selection.Selection{Types: []string{"audio"}}, RequiredTypes: []string{sitepub.Type}}} {
		if reopened, err := OpenSubscribed(path, cfg, p); !errors.Is(err, subscription.ErrMismatch) {
			if reopened != nil {
				reopened.Close()
			}
			t.Fatalf("policy mismatch accepted: %v", err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("mismatch rewrote world")
		}
	}
	*now = now.Add(2 * time.Second)
	s, err = OpenSubscribed(path, cfg, storePolicy())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := apply(t, s, c); got != world.Obsolete {
		t.Fatal("replayed record revived")
	}
	if _, found, err := s.Get(c.Key); err != nil || found {
		t.Fatal("expired tombstone visible", err)
	}
	cp, err := s.RepairSnapshot()
	if err != nil || len(cp.Entries) != 1 || !cp.Entries[0].Component.Tombstone {
		t.Fatal("retained deletion lost", err)
	}
}
func TestFullStoreCannotBecomePartialEvenWhenEmpty(t *testing.T) {
	path, cfg, _, _ := fixture(t)
	s := open(t, path, cfg)
	s.Close()
	if reopened, err := OpenSubscribed(path, cfg, storePolicy()); !errors.Is(err, subscription.ErrMismatch) {
		if reopened != nil {
			reopened.Close()
		}
		t.Fatal(err)
	}
}
func TestSubscriptionRejectsEveryAdmissionPathBeforePreparation(t *testing.T) {
	for _, kind := range []string{"direct", "prepared", "batch"} {
		t.Run(kind, func(t *testing.T) {
			path, cfg, _, c := fixture(t)
			s, err := OpenSubscribed(path, cfg, storePolicy())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			c.Key.Type = "audio"
			called := false
			switch kind {
			case "direct":
				_, err = s.Apply(c.Key.Source, c)
			case "prepared":
				_, err = s.ApplyPrepared(c.Key.Source, c, func(PublicationState) error { called = true; return nil })
			case "batch":
				_, err = s.ApplyPreparedBatch(repairPage(c), func(BatchPublicationState) error { called = true; return nil })
			}
			if !errors.Is(err, subscription.ErrExcluded) || called {
				t.Fatalf("admission %v callback %t", err, called)
			}
			records, err := s.Records()
			if err != nil || len(records) != 0 {
				t.Fatal("irrelevant bytes persisted", err)
			}
		})
	}
}
func TestSubscriptionCrashAtDurableWrite(t *testing.T) {
	if path := os.Getenv("WORLD_SUBSCRIPTION_CRASH_PATH"); path != "" {
		_, cfg, _, c := fixture(t)
		s, err := OpenSubscribed(path, cfg, storePolicy())
		if err != nil {
			t.Fatal(err)
		}
		apply(t, s, c)
		s.write = func(path string, data []byte) error {
			if os.Getenv("WORLD_SUBSCRIPTION_CRASH_AFTER") == "yes" {
				if err := writeAtomic(path, data); err != nil {
					return err
				}
			}
			os.Exit(73)
			return nil
		}
		c.Version.Sequence = 2
		c.Tombstone = true
		c.Payload = nil
		apply(t, s, c)
		t.Fatal("did not crash")
	}
	for _, after := range []string{"no", "yes"} {
		t.Run(after, func(t *testing.T) {
			path, cfg, _, c := fixture(t)
			command := exec.Command(os.Args[0], "-test.run=^TestSubscriptionCrashAtDurableWrite$")
			command.Env = append(os.Environ(), "WORLD_SUBSCRIPTION_CRASH_PATH="+path, "WORLD_SUBSCRIPTION_CRASH_AFTER="+after)
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 73 {
				t.Fatalf("child: %v %s", err, output)
			}
			s, err := OpenSubscribed(path, cfg, storePolicy())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			records, err := s.Records()
			if err != nil || len(records) != 1 {
				t.Fatal(err)
			}
			if records[0].Tombstone != (after == "yes") {
				t.Fatal("non-atomic crash state")
			}
			if after == "yes" && apply(t, s, c) != world.Obsolete {
				t.Fatal("deletion resurrected")
			}
			raw, _ := os.ReadFile(path)
			var e envelope
			json.Unmarshal(raw, &e)
			var saved state
			json.Unmarshal(e.State, &saved)
			if saved.Version != 2 || !subscription.Equal(saved.Subscription, storePolicy()) {
				t.Fatal("crash lost bound policy")
			}
		})
	}
}
