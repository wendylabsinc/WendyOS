package servicejournal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func TestRemoteAuthorizationTraceDistinguishesOriginalAndCallbackBounds(t *testing.T) {
	for _, mode := range []string{"record", "callback", "visible"} {
		t.Run(mode, func(t *testing.T) {
			f := remoteSetup(t)
			v, err := f.r.verify(context.Background(), f.raw(t))
			if err != nil {
				t.Fatal(err)
			}
			w := f.clock.w
			want := ""
			switch mode {
			case "record":
				w.Earliest = time.UnixMilli(v.Record.IssuedAtUnixMS).Add(-time.Millisecond)
				want = "remote_record_bound_omitted"
			case "callback":
				f.authorityUntil = w.Latest
				want = "remote_callback_window_omitted"
			}
			clock := &timingClock{inner: f.r.base.cfg.PhysicalClock}
			f.r.base.cfg.PhysicalClock = clock
			plain, plainErr := f.r.authorize(context.Background(), v, w)
			plainCalls := f.authorizations
			f.authorizations = 0
			sink, err := rpctrace.New(rpctrace.Config{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, finish := rpctrace.Begin(context.Background(), sink, rpctrace.Labels{Method: rpctrace.MethodQuery, App: "reader", Service: "reader", Generation: 1})
			got, gotErr := f.r.authorize(ctx, v, w)
			finish(rpctrace.OK)
			sink.Close()
			var raw bytes.Buffer
			if err := sink.Run(context.Background(), &raw); err != nil {
				t.Fatal(err)
			}
			var event rpctrace.Event
			if err := json.Unmarshal(raw.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			if got != plain || !errors.Is(gotErr, plainErr) || f.authorizations != plainCalls || clock.calls != 0 {
				t.Fatal("diagnostics changed result, callback ordering or clock reads", got, gotErr, plain, plainErr, f.authorizations, plainCalls, clock.calls)
			}
			if mode != "visible" && !errors.Is(gotErr, serviceorigin.ErrExpired) {
				t.Fatal(gotErr)
			}
			counts := map[string]uint64{}
			for _, stage := range event.Stages {
				if stage.Stage == "total" {
					continue
				}
				if stage.SumNS != 0 || stage.MaxNS != 0 {
					t.Fatal("refusal was timed", stage)
				}
				counts[stage.Stage] = stage.Count
			}
			if want == "" && len(counts) != 0 || want != "" && (len(counts) != 1 || counts[want] != 1) {
				t.Fatal(counts, want)
			}
		})
	}
}
