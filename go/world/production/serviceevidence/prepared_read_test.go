package serviceevidence

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

type nonComparableContext struct {
	context.Context
	values []int
}

func TestPreparedReadRejectsNonComparableContextWithoutPanic(t *testing.T) {
	cfg, _ := fixture(t)
	s := open(t, cfg)
	r := request([]byte("context test bytes"))
	if _, err := s.Put(context.Background(), owner(), r); err != nil {
		t.Fatal(err)
	}
	ctx := nonComparableContext{Context: context.Background(), values: []int{1}}
	if h, _, _, err := s.PrepareRead(ctx, owner(), r.SHA256, r.Size, r.ContentType); err == nil || h != nil {
		t.Fatal("non-comparable context accepted")
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _, _, err := s.PrepareRead(base, owner(), r.SHA256, r.Size, r.ContentType)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	w, _ := cfg.Clock.Now()
	if err = s.ValidatePrepared(ctx, h, w); err == nil {
		t.Fatal("non-comparable validation context accepted")
	}
}

func TestPreparedReadOwnsFactsAndRequiresSameOperationAndStore(t *testing.T) {
	cfg, clock := fixture(t)
	s := open(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := []byte("immutable original media")
	req := request(body)
	put, err := s.Put(ctx, owner(), req)
	if err != nil {
		t.Fatal(err)
	}
	h, ref, data, err := s.PrepareRead(ctx, owner(), req.SHA256, req.Size, req.ContentType)
	if err != nil || !bytes.Equal(data, body) {
		t.Fatal(err)
	}
	defer h.Close()
	ref.RetainUntilUnixMS++
	data[0] ^= 1
	w, _ := clock.Now()
	before := clock.calls
	for range 3 {
		if err = s.ValidatePrepared(ctx, h, w); err != nil {
			t.Fatal(err)
		}
	}
	if clock.calls != before {
		t.Fatal("metadata validation read a clock")
	}
	if err = s.ValidatePrepared(context.Background(), h, w); err == nil {
		t.Fatal("cross-operation handle accepted")
	}
	otherCfg, _ := fixture(t)
	other := open(t, otherCfg)
	if err = other.ValidatePrepared(ctx, h, w); err == nil {
		t.Fatal("cross-store handle accepted")
	}
	got, e := s.Get(ctx, owner(), GetRequest{SHA256: req.SHA256, Length: MaxChunk})
	if e != nil || !bytes.Equal(got.Data, body) || got.RetainUntilUnixMS != put.RetainUntilUnixMS {
		t.Fatal("returned aliases changed store", e)
	}
	h.Close()
	if err = s.ValidatePrepared(ctx, h, w); err == nil {
		t.Fatal("closed handle accepted")
	}
}

func TestPreparedReadRejectsFaultExpiryReallocationAndCancellation(t *testing.T) {
	for _, mode := range []string{"cancel", "store_closed", "inner_closed", "outer_fault", "expired_reallocated"} {
		t.Run(mode, func(t *testing.T) {
			cfg, clock := fixture(t)
			s := open(t, cfg)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := request([]byte("same digest before and after expiry"))
			r.RetainUntilUnixMS = clock.w.Latest.Add(time.Second).UnixMilli()
			if _, e := s.Put(ctx, owner(), r); e != nil {
				t.Fatal(e)
			}
			h, _, _, e := s.PrepareRead(ctx, owner(), r.SHA256, r.Size, r.ContentType)
			if e != nil {
				t.Fatal(e)
			}
			defer h.Close()
			w, _ := clock.Now()
			switch mode {
			case "cancel":
				cancel()
			case "store_closed":
				_ = s.Close()
			case "inner_closed":
				_ = s.inner.Close()
			case "outer_fault":
				s.fault = errors.New("test fault")
			case "expired_reallocated":
				clock.advance(2 * time.Second)
				r.RetainUntilUnixMS = clock.w.Latest.Add(time.Minute).UnixMilli()
				if _, e = s.Put(ctx, owner(), r); e != nil {
					t.Fatal(e)
				}
				// Even a supplied old interval cannot resurrect the old allocation.
			}
			if e = s.ValidatePrepared(ctx, h, w); e == nil {
				t.Fatal("invalid handle accepted")
			}
		})
	}
}
