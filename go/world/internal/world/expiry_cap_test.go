package world

import (
	"errors"
	"testing"
	"time"
)

func TestClampExpiryExactVersionOnlyAndNoExtension(t *testing.T) {
	r, now, c := fixture(t, false)
	base := *now
	apply(t, r, c)
	if err := r.ClampExpiry(c.Key, c.Version, base.Add(200*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := r.ClampExpiry(c.Key, c.Version, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cp, err := r.Checkpoint()
	if err != nil || !cp.Entries[0].ExpiresAt.Equal(base.Add(200*time.Millisecond)) {
		t.Fatal("cap extended", cp, err)
	}
	c.Version.Sequence++
	apply(t, r, c)
	old := c.Version
	old.Sequence--
	if err := r.ClampExpiry(c.Key, old, base.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	cp, err = r.Checkpoint()
	if err != nil || !cp.Entries[0].ExpiresAt.Equal(base.Add(time.Second)) {
		t.Fatal("obsolete cap touched newer version", cp, err)
	}
	if err := r.ClampExpiry(c.Key, c.Version, time.Time{}); err == nil {
		t.Fatal("zero cap accepted")
	}
	missing := c.Key
	missing.Entity = "absent"
	if err := r.ClampExpiry(missing, c.Version, base); err == nil {
		t.Fatal("missing key accepted")
	}
	*now = base.Add(-time.Nanosecond)
	if err := r.ClampExpiry(c.Key, c.Version, base); !errors.Is(err, ErrClockRegression) {
		t.Fatal(err)
	}
}

func TestClampExpiryRealClockPreservesMonotonicDeadline(t *testing.T) {
	r, _, c := fixture(t, false)
	r.config.Now = time.Now
	c.Timing.SampleTime = time.Now()
	apply(t, r, c)
	cap := time.Now().Add(100 * time.Millisecond).Round(0)
	if err := r.ClampExpiry(c.Key, c.Version, cap); err != nil {
		t.Fatal(err)
	}
	cp, err := r.Checkpoint()
	if err != nil || !cp.Entries[0].ExpiresAt.Equal(cap) {
		t.Fatal("wall cap changed", err)
	}
	clone, err := r.Clone()
	if err != nil {
		t.Fatal(err)
	}
	cp, err = clone.Checkpoint()
	if err != nil || !cp.Entries[0].ExpiresAt.Equal(cap) {
		t.Fatal("clone extended cap", err)
	}
}
