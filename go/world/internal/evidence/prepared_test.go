package evidence

import (
	"errors"
	"testing"
	"time"
)

func TestPreparedProtectedMetadata(t *testing.T) {
	now := time.Now()
	s, err := Open(t.TempDir(), config(&now))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := request([]byte("immutable protected bytes"))
	put, err := s.Put("camera", r)
	if err != nil {
		t.Fatal(err)
	}
	ref := Reference{SHA256: r.SHA256, Size: r.Size, ContentType: r.ContentType}
	if err = s.ValidateProtectedOwned("camera", ref, put.RetainUntilUnixMS); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []func(){
		func() { m := s.entries[r.SHA256]; m.Owner = "other"; s.entries[r.SHA256] = m },
		func() { m := s.entries[r.SHA256]; m.RetainUntilUnixMS++; s.entries[r.SHA256] = m },
		func() { m := s.entries[r.SHA256]; m.RetentionClass = RetentionRolling; s.entries[r.SHA256] = m },
		func() { m := s.entries[r.SHA256]; m.Complete = false; s.entries[r.SHA256] = m },
		func() { delete(s.entries, r.SHA256) },
	} {
		original := s.entries[r.SHA256]
		mutation()
		if err = s.ValidateProtectedOwned("camera", ref, put.RetainUntilUnixMS); err == nil {
			t.Fatal("changed metadata accepted")
		}
		s.entries[r.SHA256] = original
	}
	s.fault = errors.New("detected inner storage fault")
	if err = s.ValidateProtectedOwned("camera", ref, put.RetainUntilUnixMS); err == nil {
		t.Fatal("fault ignored")
	}
	s.fault = nil
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateProtectedOwned("camera", ref, put.RetainUntilUnixMS); err == nil {
		t.Fatal("closed store accepted")
	}
}
