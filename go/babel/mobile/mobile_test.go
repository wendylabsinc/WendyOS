package mobile

import (
	"encoding/json"
	"testing"
)

func TestFacade(t *testing.T) {
	e, err := New("1", 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Step(0, []byte(`{"Type":"originate","Prefix":"fd00::1/128"}`))
	if err != nil {
		t.Fatal(err)
	}
	var fx struct{ Revision int64 }
	if err = json.Unmarshal(b, &fx); err != nil || fx.Revision == 0 {
		t.Fatal(string(b), err)
	}
	if _, err = e.Commit(fx.Revision, true); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Step(1, []byte(`{"Type":"tick"} {}`)); err == nil {
		t.Fatal("trailing input")
	}
	if _, err = e.Step(1, []byte(`{"Type":"tick","Mystery":1}`)); err == nil {
		t.Fatal("unknown field")
	}
}

func TestCheckpointFacade(t *testing.T) {
	e, err := New("1", 7)
	if err != nil {
		t.Fatal(err)
	}
	if e.NextDeadlineMS() != -1 {
		t.Fatal("idle timer")
	}
	s, err := e.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	r, err := Restore(s)
	if err != nil {
		t.Fatal(err)
	}
	data, err := r.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var snap struct{ Seqno int }
	if err = json.Unmarshal(data, &snap); err != nil || snap.Seqno != 7 {
		t.Fatal(string(data), err)
	}
}
