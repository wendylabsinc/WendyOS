package world

import (
	"errors"
	"testing"
	"time"
)

func TestCheckpointRestoresAllStateWithoutAliasing(t *testing.T) {
	r, now, c := fixture(t, false)
	apply(t, r, c)
	cp, err := r.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Second)
	restored, err := Restore(r.config, cp)
	if err != nil {
		t.Fatal(err)
	}
	if visible(t, restored, c) {
		t.Fatal("expired restored value visible")
	}
	if got := apply(t, restored, c); got != Duplicate {
		t.Fatal(got)
	}
	cp.Entries[0].Component.Payload[5] = '9'
	records, err := restored.Records()
	if err != nil {
		t.Fatal(err)
	}
	if string(records[0].Payload) != `{"x":1}` {
		t.Fatal("checkpoint aliases live state")
	}
}
func TestCheckpointRejectsExtendedExpiryAndDuplicateKeys(t *testing.T) {
	r, _, c := fixture(t, false)
	apply(t, r, c)
	cp, err := r.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	cp.Entries[0].ExpiresAt = cp.Entries[0].ExpiresAt.Add(time.Second)
	if _, err := Restore(r.config, cp); err == nil {
		t.Fatal("extended expiry accepted")
	}
	cp, err = r.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	cp.Entries = append(cp.Entries, cp.Entries[0])
	if _, err := Restore(r.config, cp); err == nil {
		t.Fatal("duplicate key accepted")
	}
}
func TestCheckpointRejectsRollback(t *testing.T) {
	r, now, c := fixture(t, false)
	apply(t, r, c)
	cp, err := r.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(-time.Second)
	if _, err := Restore(r.config, cp); !errors.Is(err, ErrClockRegression) {
		t.Fatal(err)
	}
}
