package builddelivery

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDurableAdmissionSurvivesOperatorExpiryAndPinsOutput(t *testing.T) {
	f := newFixture(t)
	clock := f.now
	dir := t.TempDir()
	s, err := Open(dir, f.roots, f.target, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	block, _ := pem.Decode([]byte(f.op))
	operator, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	compact := f.sign(t, f.claims)
	id, err := s.Admit(compact, operator)
	if err != nil {
		t.Fatal(err)
	}
	after := f.now.Add(20 * time.Minute)
	clock = after
	s.Close()
	s, err = Open(dir, f.roots, f.target, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authorize(id, f.builder, f.claims.Reference); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authorize(id, operator, ""); err == nil {
		t.Fatal("operator substituted for bound builder")
	}
	other, key := f.mint(f.claims.Builder, nil)
	_ = key
	block, _ = pem.Decode([]byte(other))
	rotated, _ := x509.ParseCertificate(block.Bytes)
	if _, err = s.Authorize(id, rotated, ""); err == nil {
		t.Fatal("rotated builder key inherited grant")
	}
	if _, err = s.Authorize(id, f.builder, "another:tag"); err == nil {
		t.Fatal("reference expanded")
	}
	output := "sha256:" + strings.Repeat("a", 64)
	calls := 0
	fail := errors.New("publication interrupted")
	apply := func(expected, got string) error {
		calls++
		if expected != "" || got != output {
			t.Fatal("lost CAS/output binding")
		}
		if calls == 1 {
			return fail
		}
		return nil
	}
	if err = s.Publish(id, f.builder, f.claims.Reference, output, apply); !errors.Is(err, fail) {
		t.Fatalf("publish %v", err)
	}
	s.Close()
	s, err = Open(dir, f.roots, f.target, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Publish(id, f.builder, f.claims.Reference, "sha256:"+strings.Repeat("b", 64), apply); err == nil {
		t.Fatal("changed pending output")
	}
	if _, err = s.Authorize(id, f.builder, ""); err != nil {
		t.Fatal("pending preparation blocked concurrent chunks", err)
	}
	if err = s.Publish(id, f.builder, f.claims.Reference, output, apply); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("unexpected publication calls")
	}
	if _, err = s.Authorize(id, f.builder, ""); err == nil {
		t.Fatal("staging continued after completed publication")
	}
	if err = s.Publish(id, f.builder, f.claims.Reference, output, apply); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("replay repeated a completed image mutation")
	}
	deadline := time.Unix(f.claims.ValidUntil, 0)
	clock = deadline
	if err = s.Publish(id, f.builder, f.claims.Reference, output, apply); err == nil {
		t.Fatal("publication after expiry")
	}
	if calls != 2 {
		t.Fatal("expired publication invoked backend")
	}
}
func TestJobReplayAndDurableBudget(t *testing.T) {
	f := newFixture(t)
	f.claims.MaxBytes = 8
	clock := f.now
	dir := t.TempDir()
	s, err := Open(dir, f.roots, f.target, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	j := f.sign(t, f.claims)
	if _, err = s.Admit(j, nil); err == nil {
		t.Fatal("anonymous admission")
	}
	id, err := s.Admit(j, f.builder)
	if err != nil {
		t.Fatal(err)
	}
	clock = f.now.Add(2 * time.Minute)
	if again, err := s.Admit(j, f.builder); err != nil || again != id {
		t.Fatalf("retry must use original admission: %s %v", again, err)
	}
	clock = f.now
	c := f.claims
	c.Reference = "other:tag"
	if _, err = s.Admit(f.sign(t, c), f.builder); err == nil {
		t.Fatal("job replay with different scope")
	}
	if err = s.ReserveBytes(id, f.builder, 5); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir, f.roots, f.target, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReserveBytes(id, f.builder, 4); err == nil {
		t.Fatal("restart reset byte budget")
	}
	if err = s.ReserveBytes(id, f.builder, 3); err != nil {
		t.Fatal(err)
	}
	if err = s.ReserveBytes(id, f.builder, -1); err == nil {
		t.Fatal("negative byte budget")
	}
}

func TestStoreExcludesConcurrentProcessesAndFailsClosedAfterClose(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	clock := func() time.Time { return f.now }
	s, err := Open(dir, f.roots, f.target, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if other, err := Open(dir, f.roots, f.target, clock); err == nil {
		other.Close()
		t.Fatal("two stores could mutate replay state")
	}
	s.Close()
	if _, err = s.Admit(f.sign(t, f.claims), f.builder); err == nil {
		t.Fatal("closed store admitted authorization")
	}
	called := false
	if err = s.Direct(func() error { called = true; return nil }); err == nil || called {
		t.Fatal("closed store allowed image mutation")
	}
	reopened, err := Open(dir, f.roots, f.target, clock)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
}
