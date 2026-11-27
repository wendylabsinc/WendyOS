package hazardreader

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type clockClient struct {
	appv1.WorldAppServiceClient
	clocks       int
	finalFailure string
}

func (c *clockClient) Clock(ctx context.Context, request *appv1.ClockRequest, opts ...grpc.CallOption) (*appv1.ClockReply, error) {
	c.clocks++
	if c.clocks == 2 && c.finalFailure == "unavailable" {
		return nil, status.Error(codes.Unavailable, "clock unavailable")
	}
	reply, err := c.WorldAppServiceClient.Clock(ctx, request, opts...)
	if err == nil && c.clocks == 2 && c.finalFailure == "signing-expired" {
		reply.Physical.EarliestUnixNs += int64(61 * time.Second)
		reply.Physical.LatestUnixNs += int64(61 * time.Second)
		reply.Physical.MonotonicNs += int64(61 * time.Second)
	}
	return reply, err
}

func assertClock(t *testing.T, r Result, f *fixture) {
	t.Helper()
	c := r.Clock
	if c == nil || !c.SampleTime.Equal(r.SampleTime) || !c.SampleTime.Equal(f.now) ||
		c.SampleClockDomain != "simulation:"+f.cfg.RunID || c.DeploymentGeneration != r.ReaderGeneration ||
		c.DeploymentGeneration != 9 || c.ServerBootID != strings.Repeat("a", 32) ||
		!c.PhysicalEarliest.Equal(f.w.Earliest) || c.PhysicalLatest.Before(f.w.Latest) {
		t.Fatalf("clock does not describe this read: %+v", c)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var object struct {
		Clock map[string]json.RawMessage `json:"clock"`
	}
	if err = json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(object.Clock))
	for key := range object.Clock {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := []string{"deployment_generation", "physical_earliest", "physical_latest", "sample_clock_domain", "sample_time", "server_boot_id"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("clock wire fields: %v", keys)
	}
}

func TestHazardClockUsesFinalObservationWithoutAnotherRPC(t *testing.T) {
	for _, empty := range []bool{false, true} {
		name := "present"
		if empty {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			f, api := setup(t)
			initial := f.now
			if empty {
				f.raw = nil
			}
			f.queryHook = func(int) { f.now = initial.Add(100 * time.Millisecond) }
			client := &clockClient{WorldAppServiceClient: api}
			s, err := New(f.cfg, client)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r, err := s.Step(context.Background())
			wantHazards, wantQueries := 1, 3
			if empty {
				wantHazards, wantQueries = 0, 1
			}
			if err != nil || r.Status != "ready" || len(r.Hazards) != wantHazards ||
				client.clocks != 2 || f.queries != wantQueries || r.SampleTime.Equal(initial) {
				t.Fatalf("final read changed: status=%s hazards=%d clocks=%d queries=%d error=%v", r.Status, len(r.Hazards), client.clocks, f.queries, err)
			}
			assertClock(t, r, f)
		})
	}
}

func TestHazardClockWaitingUsesInitialObservation(t *testing.T) {
	f, api := setup(t)
	f.denied = 1
	client := &clockClient{WorldAppServiceClient: api}
	s, err := New(f.cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := s.Step(context.Background())
	if err == nil || r.Status != "waiting_for_source" || client.clocks != 1 ||
		len(r.Hazards) != 0 || len(r.AbsentHazardIDs) != 0 || r.OriginAttestedInference {
		t.Fatal("waiting claimed a completed read", err)
	}
	assertClock(t, r, f)
}

func TestHazardClockOmittedAfterTerminalFailure(t *testing.T) {
	for _, mode := range []string{"unavailable", "signing-expired", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			f, api := setup(t)
			if mode == "malformed" {
				f.raw = []byte(`{}`)
			}
			client := &clockClient{WorldAppServiceClient: api, finalFailure: mode}
			s, err := New(f.cfg, client)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r, err := s.Step(context.Background())
			if err == nil || r.Status != "failed" || r.Clock != nil || len(r.Hazards) != 0 || r.OriginAttestedInference {
				t.Fatal("terminal failure retained a current clock/read", err)
			}
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]json.RawMessage
			if err = json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if _, exists := object["clock"]; exists {
				t.Fatal("failed JSON includes clock")
			}
			want := 2
			if mode == "malformed" {
				want = 1
			}
			if client.clocks != want {
				t.Fatalf("unexpected Clock RPC count: %d", client.clocks)
			}
			if next, err := s.Step(context.Background()); err == nil || next.Clock != nil || client.clocks != want {
				t.Fatal("terminal result restarted or reused clock")
			}
		})
	}
}
