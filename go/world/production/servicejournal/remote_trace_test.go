package servicejournal

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func tracedRemoteSnapshot(t *testing.T, f *remoteEnvironment) ([]RecordExport, map[string]uint64) {
	t.Helper()
	sink, err := rpctrace.New(rpctrace.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, finish := rpctrace.Begin(context.Background(), sink, rpctrace.Labels{Method: rpctrace.MethodQuery, App: "com.example.reader", Service: "reader", Generation: 1})
	out, err := f.r.Snapshot(ctx)
	finish(rpctrace.OK)
	if err != nil {
		t.Fatal(err)
	}
	sink.Close()
	var raw bytes.Buffer
	if err := sink.Run(context.Background(), &raw); err != nil {
		t.Fatal(err)
	}
	var event rpctrace.Event
	if err := json.Unmarshal(raw.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	counts := map[string]uint64{}
	for _, stage := range event.Stages {
		if len(stage.Stage) >= 7 && stage.Stage[:7] == "remote_" {
			if stage.SumNS != 0 || stage.MaxNS != 0 {
				t.Fatal("branch count performed timing", stage)
			}
			counts[stage.Stage] = stage.Count
		}
	}
	return out, counts
}

func TestRemoteSnapshotTraceReportsExistingOmissionBranches(t *testing.T) {
	for _, mode := range []string{"visible", "verify-first", "verify-second", "authority-first", "authority-second", "membership-final", "record-final"} {
		t.Run(mode, func(t *testing.T) {
			f := remoteSetup(t)
			raw := f.raw(t)
			f.admitRemote(t, raw, world.Applied)
			f.authorizations = 0
			schemaCalls := 0
			originalSchema := f.r.base.cfg.AdmitSchema
			f.r.base.cfg.AdmitSchema = func(ctx context.Context, v serviceorigin.VerifiedRecord) error {
				schemaCalls++
				if mode == "verify-first" || mode == "verify-second" && schemaCalls == 2 {
					return serviceorigin.ErrDenied
				}
				return originalSchema(ctx, v)
			}
			if mode == "authority-first" {
				f.allowed = false
			}
			f.authorizeHook = func(n int, _ serviceorigin.VerifiedRecord) {
				if n != 2 {
					return
				}
				switch mode {
				case "authority-second":
					f.allowed = false
				case "membership-final":
					f.sourceNow = f.request.Timing.SampleTime.Add(f.request.TTL)
				case "record-final":
					f.clock.advance(11 * time.Minute)
				}
			}
			out, counts := tracedRemoteSnapshot(t, f)
			want := map[string]uint64{"remote_candidate": 1}
			switch mode {
			case "visible":
				want["remote_visible"] = 1
				if len(out) != 1 || !bytes.Equal(out[0].Envelope, raw) || schemaCalls != 2 || f.authorizations != 2 {
					t.Fatal("diagnostics changed current original or checks")
				}
			case "verify-first", "verify-second":
				want["remote_verify_omitted"] = 1
			case "authority-first", "authority-second":
				want["remote_authority_omitted"] = 1
			case "membership-final":
				want["remote_membership_omitted"] = 1
			case "record-final":
				want["remote_record_omitted"] = 1
			}
			if mode != "visible" && len(out) != 0 || !reflect.DeepEqual(counts, want) {
				t.Fatalf("branch %s: returned=%d got=%v want=%v", mode, len(out), counts, want)
			}
		})
	}
}

func TestRemoteSnapshotTraceFinalWindowUsesExistingCommonObservation(t *testing.T) {
	f := remoteSetup(t)
	f.admitRemote(t, f.raw(t), world.Applied)
	f.request.Entity = "second"
	f.admitRemote(t, f.raw(t), world.Applied)
	start := f.clock.w.Earliest
	f.authorityUntil = start.Add(5 * time.Second)
	f.authorizations = 0
	f.authorizeHook = func(n int, _ serviceorigin.VerifiedRecord) {
		if n == 4 {
			f.clock.advance(6 * time.Second)
			f.authorityUntil = start.Add(time.Hour)
		}
	}
	out, counts := tracedRemoteSnapshot(t, f)
	if len(out) != 1 || f.authorizations != 4 || !reflect.DeepEqual(counts, map[string]uint64{"remote_candidate": 2, "remote_window_omitted": 1, "remote_visible": 1}) {
		t.Fatal("common-window observation changed", len(out), f.authorizations, counts)
	}
}
