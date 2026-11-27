package serviceorigin

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestAuthorityWindowIsImmutableMetadataNotAClockLease(t *testing.T) {
	e := setup(t)
	signed := e.publish(t)
	var r Record
	if err := json.Unmarshal(signed.Record, &r); err != nil {
		t.Fatal(err)
	}
	before := e.clock.calls
	from, until, err := e.publisher.AuthorityWindow()
	if err != nil || e.clock.calls != before || from.UnixMilli() != r.IssuedAtUnixMS || until.UnixMilli() != r.ExpiresAtUnixMS {
		t.Fatal("authority metadata changed window or sampled a clock", err)
	}
	if _, err = e.cfg.Certificates.Verify(signed.Delegation.ChainDER, from, until); err != nil {
		t.Fatal("window escaped device certificate path", err)
	}
	until = until.Add(time.Hour)
	_, again, err := e.publisher.AuthorityWindow()
	if err != nil || !again.Before(until) {
		t.Fatal("caller changed publisher window")
	}
	e.publisher.Close()
	if from, until, err := e.publisher.AuthorityWindow(); !errors.Is(err, ErrClosed) || !from.IsZero() || !until.IsZero() {
		t.Fatal("closed publisher supplied active metadata", err)
	}
}
