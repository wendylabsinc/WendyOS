//go:build darwin || linux || windows

package commands

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func TestTourRobotProjectResumesWithoutOverwritingEdits(t *testing.T) {
	dir := t.TempDir()
	p, path, err := loadTourRobotProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	p.TurnRPS = -.4
	data, _ := json.Marshal(p)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, _, err := loadTourRobotProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Fatalf("project changed on resume: %+v != %+v", got, p)
	}
	if filepath.Base(path) != "tour.json" {
		t.Fatal(path)
	}
}

func TestTourRobotProjectRejectsUnboundedMotion(t *testing.T) {
	p := tourRobotProject{Version: 1, VMName: "tour-test", DurationSeconds: 5, ForwardMPS: .55, TurnRPS: .5}
	for _, mutate := range []func(*tourRobotProject){func(p *tourRobotProject) { p.DurationSeconds = 100 }, func(p *tourRobotProject) { p.ForwardMPS = 2 }, func(p *tourRobotProject) { p.TurnRPS = 2 }, func(p *tourRobotProject) { p.VMName = "192.168.1.42" }} {
		bad := p
		mutate(&bad)
		if bad.validate() == nil {
			t.Fatalf("accepted unsafe settings %+v", bad)
		}
	}
}

func TestTourRobotWalkRequiresObservedMotionAndAlwaysReleases(t *testing.T) {
	for _, test := range []struct {
		name                   string
		move, cancel, mismatch bool
		wantErr                bool
	}{
		{"walk", true, false, false, false}, {"commands without movement", false, false, false, true},
		{"cancel", true, true, false, true}, {"wrong identity", true, false, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile, err := vm.NewGo2RobotProfile("sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", go2PolicyBundle)
			if err != nil {
				t.Fatal(err)
			}
			p := tourRobotProject{Version: 1, VMName: "tour-test", DurationSeconds: 1, ForwardMPS: .55, TurnRPS: .5}
			o := tourRobotObservation{robotRuntimeStatus: robotRuntimeStatus{VMName: p.VMName, Simulation: true, ProfileVersion: profile.Version, RobotKind: profile.Kind, SourceDigest: profile.SourceDigest, PolicyBundle: profile.PolicyBundle, World: profile.World, ClockMode: profile.ClockMode, Seed: profile.Seed, VisualDetail: profile.VisualDetail, DDSIsolation: "udp-rtps-loopback", Healthy: true, Ready: true}, Position: []float64{0, 0, .34}, Velocity: []float64{0, 0, 0}, Command: []float64{0, 0, 0}, Time: 1}
			if test.mismatch {
				o.Simulation = false
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var mu sync.Mutex
			posts := map[string]int{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == http.MethodPost {
					posts[r.URL.Path]++
				}
				switch r.URL.Path {
				case "/api/status":
					_ = json.NewEncoder(w).Encode(o)
				case "/api/reset":
					o.Epoch++
					_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
				case "/api/arm":
					o.Armed = true
					_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-lease"})
				case "/api/command":
					var body struct {
						Token    string    `json:"token"`
						Velocity []float64 `json:"velocity"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body.Token != "test-lease" {
						t.Error("missing owned lease")
					}
					o.Command = body.Velocity
					o.Time += .1
					if test.move {
						o.Position[0] += .1
					}
					if test.cancel {
						cancel()
					}
					_ = json.NewEncoder(w).Encode(map[string]bool{"accepted": true})
				case "/api/stop":
					o.Command = []float64{0, 0, 0}
					_ = json.NewEncoder(w).Encode(map[string]bool{"stopped": true})
				case "/api/release":
					o.Armed = false
					_ = json.NewEncoder(w).Encode(map[string]bool{"released": true})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
			port, _ := strconv.Atoi(portStr)
			distance, err := walkTourRobot(ctx, port, profile, p)
			if (err != nil) != test.wantErr {
				t.Fatalf("distance=%v error=%v", distance, err)
			}
			if test.cancel && !errors.Is(err, ErrUserCancelled) && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if test.mismatch {
				if len(posts) != 0 {
					t.Fatalf("sent control to wrong identity: %v", posts)
				}
				return
			}
			if posts["/api/stop"] != 1 || posts["/api/release"] != 1 || o.Armed {
				t.Fatalf("did not stop and release: %v armed=%v", posts, o.Armed)
			}
			if !test.wantErr && distance < .1 {
				t.Fatalf("no motion verified: %v", distance)
			}
		})
	}
}
