package jws

import (
	"testing"
	"time"
)

func TestDecodeStrictStandardTimeOnly(t *testing.T) {
	type body struct {
		Sample   time.Time  `json:"sample"`
		Optional *time.Time `json:"optional,omitempty"`
	}
	var value body
	if err := DecodeStrict([]byte(`{"sample":"2026-11-10T12:00:00.123Z","optional":"2026-11-10T12:00:01Z"}`), &value); err != nil {
		t.Fatal(err)
	}
	if value.Sample.Nanosecond() != 123000000 || value.Optional == nil {
		t.Fatal(value)
	}
	for _, raw := range []string{`{"sample":null}`, `{"sample":123}`, `{"sample":{}}`, `{"sample":"bad"}`, `{"sample":"2026-11-10T12:00:00Z","Sample":"2026-11-10T12:00:01Z"}`, `{"sample":"2026-11-10T12:00:00Z","sample":"2026-11-10T12:00:01Z"}`} {
		if err := DecodeStrict([]byte(raw), &value); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
