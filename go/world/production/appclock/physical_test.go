package appclock

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPhysicalOnlyUsesFreshRPCAndConservativeTransitWithoutSource(t *testing.T) {
	f := newFixture(t)
	f.reply.SampleUnixNs, f.reply.SampleClockDomain = 0, ""
	f.delay = 700 * time.Millisecond
	p := f.provider(t)
	o, err := p.PhysicalObserve(t.Context())
	if err != nil || o.Physical.Earliest.UnixNano() != f.reply.Physical.EarliestUnixNs || o.Physical.Latest.UnixNano() != f.reply.Physical.LatestUnixNs+int64(f.delay+driftBound(f.delay, f.cfg.DriftPPM)) || o.RoundTrip != f.delay || f.combinedCalls != 0 || f.physicalCalls != 1 {
		t.Fatal("physical read used source time or lost transit", err)
	}
	if _, err = p.Now(); err != nil || f.physicalCalls != 2 || f.combinedCalls != 0 {
		t.Fatal("Now reused authority or queried source", err)
	}
	if _, err = p.Observe(t.Context()); !errors.Is(err, ErrFaulted) {
		t.Fatal("combined reply accepted missing source fields")
	}
}

func TestPhysicalFaultsShareIdentityAndContinuityWithBothMethods(t *testing.T) {
	cases := map[string]func(*appv1.PhysicalClockReply){
		"nonce":                    func(r *appv1.PhysicalClockReply) { r.Nonce[0] ^= 1 },
		"tenant":                   func(r *appv1.PhysicalClockReply) { r.Tenant = "3dc4fd79-fc22-4baf-8047-6eb4ca9c03b9" },
		"site":                     func(r *appv1.PhysicalClockReply) { r.Site = "elsewhere" },
		"device":                   func(r *appv1.PhysicalClockReply) { r.Device = strings.TrimSuffix(r.Device, "b02") + "b03" },
		"app":                      func(r *appv1.PhysicalClockReply) { r.App = "com.example.other" },
		"service":                  func(r *appv1.PhysicalClockReply) { r.Service = "other" },
		"domain":                   func(r *appv1.PhysicalClockReply) { r.ExecutionDomain = "physical" },
		"generation":               func(r *appv1.PhysicalClockReply) { r.DeploymentGeneration++ },
		"zero-generation":          func(r *appv1.PhysicalClockReply) { r.DeploymentGeneration = 0 },
		"module":                   func(r *appv1.PhysicalClockReply) { r.ServerBootId = strings.Repeat("b", 32) },
		"host":                     func(r *appv1.PhysicalClockReply) { r.Physical.BootId = "other" },
		"source":                   func(r *appv1.PhysicalClockReply) { r.Physical.Source = "different-authenticated-source" },
		"untrusted-source":         func(r *appv1.PhysicalClockReply) { r.Physical.Source = "simulation" },
		"monotonic":                func(r *appv1.PhysicalClockReply) { r.Physical.MonotonicNs-- },
		"earliest":                 func(r *appv1.PhysicalClockReply) { r.Physical.EarliestUnixNs-- },
		"latest":                   func(r *appv1.PhysicalClockReply) { r.Physical.LatestUnixNs-- },
		"missing":                  func(r *appv1.PhysicalClockReply) { r.Physical = nil },
		"top-unknown":              func(r *appv1.PhysicalClockReply) { r.ProtoReflect().SetUnknown([]byte{0xf8, 0x01, 0x01}) },
		"nested-unknown":           func(r *appv1.PhysicalClockReply) { r.Physical.ProtoReflect().SetUnknown([]byte{0xf8, 0x01, 0x01}) },
		"sample-field-in-physical": func(r *appv1.PhysicalClockReply) { r.ProtoReflect().SetUnknown([]byte{0x20, 0x01}) },
		"width":                    func(r *appv1.PhysicalClockReply) { r.Physical.LatestUnixNs += int64(4 * time.Second) },
		"overflow": func(r *appv1.PhysicalClockReply) {
			r.Physical.EarliestUnixNs = math.MaxInt64 - int64(time.Millisecond)
			r.Physical.LatestUnixNs = math.MaxInt64
		},
	}
	for _, first := range []string{"physical", "combined"} {
		for name, mutate := range cases {
			t.Run(first+"/"+name, func(t *testing.T) {
				f := newFixture(t)
				p := f.provider(t)
				var err error
				if first == "physical" {
					_, err = p.PhysicalObserve(t.Context())
				} else {
					_, err = p.Observe(t.Context())
				}
				if err != nil {
					t.Fatal(err)
				}
				f.mutatePhysical = mutate
				if o, e := p.PhysicalObserve(t.Context()); !errors.Is(e, ErrFaulted) || o != (PhysicalObservation{}) {
					t.Fatal("physical fault returned authority", e)
				}
				f.mutatePhysical = nil
				calls := f.calls
				if _, e := p.Observe(t.Context()); !errors.Is(e, ErrFaulted) || calls != f.calls {
					t.Fatal("combined method escaped sticky physical fault", e)
				}
			})
		}
	}
}

func TestPhysicalFirstPinsCombinedAndInterleavedSourceCannotRegress(t *testing.T) {
	for _, change := range []string{"generation", "module", "host", "physical"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			f.cfg.ExpectedGeneration = 0
			p := f.provider(t)
			if _, err := p.PhysicalObserve(t.Context()); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "generation":
				f.reply.DeploymentGeneration++
			case "module":
				f.reply.ServerBootId = strings.Repeat("b", 32)
			case "host":
				f.reply.Physical.BootId = "changed"
			case "physical":
				f.reply.Physical.LatestUnixNs--
			}
			if _, err := p.Observe(t.Context()); !errors.Is(err, ErrFaulted) {
				t.Fatal("first combined read repinned prior physical authority", err)
			}
		})
	}
	f := newFixture(t)
	p := f.provider(t)
	if _, err := p.PhysicalObserve(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.reply.SampleUnixNs -= int64(time.Hour)
	first, err := p.Observe(t.Context())
	if err != nil {
		t.Fatal("physical read invented an earlier source observation", err)
	}
	f.reply.SampleUnixNs = first.SampleTime.UnixNano() - 1
	if _, err = p.PhysicalObserve(t.Context()); err != nil {
		t.Fatal("physical read interpreted source fields", err)
	}
	if _, err = p.Observe(t.Context()); !errors.Is(err, ErrFaulted) {
		t.Fatal("intervening physical read erased source high-water", err)
	}
}

type physicalUnavailable struct{ *clockFixture }

func (c physicalUnavailable) PhysicalClock(context.Context, *appv1.ClockRequest, ...grpc.CallOption) (*appv1.PhysicalClockReply, error) {
	c.physicalCalls++
	return nil, status.Error(codes.Unimplemented, "older agent")
}
func TestPhysicalMissingMethodHasNoFallbackAndNoCachedTime(t *testing.T) {
	f := newFixture(t)
	p, err := newProvider(physicalUnavailable{f}, f.cfg, f.read, strings.NewReader(strings.Repeat("x", 128)))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err = p.Observe(t.Context()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if w, e := p.Now(); !errors.Is(e, ErrUnavailable) || w != (trustedtime.Interval{}) {
			t.Fatal("physical failure returned cached or combined time", e)
		}
	}
	if f.combinedCalls != 1 || f.physicalCalls != 2 {
		t.Fatal("physical method fallback or caching")
	}
}
