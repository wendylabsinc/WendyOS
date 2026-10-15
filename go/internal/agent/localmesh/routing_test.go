package localmesh

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/wendylabsinc/WendyOS/babel"
)

type routingRecorder struct {
	calls   []string
	failure string
}

func (r *routingRecorder) call(s string) error {
	r.calls = append(r.calls, s)
	if s == r.failure {
		return errors.New("injected " + s)
	}
	return nil
}
func (r *routingRecorder) Apply([]babel.Route) error      { return r.call("apply") }
func (r *routingRecorder) Persist(babel.Checkpoint) error { return r.call("persist") }
func (r *routingRecorder) Resume() error                  { return r.call("resume") }
func (r *routingRecorder) Stop() error                    { return r.call("stop") }
func (r *routingRecorder) Send(babel.Datagram) error      { return r.call("send") }

func TestRoutingBarrierFailureStops(t *testing.T) {
	for _, failure := range []string{"apply", "persist", "resume"} {
		t.Run(failure, func(t *testing.T) {
			io := &routingRecorder{failure: failure}
			r, err := NewRouting(babel.Config{RouterID: 1}, nil, io)
			if err != nil {
				t.Fatal(err)
			}
			err = r.Step(0, babel.Originate{Prefix: netip.MustParsePrefix("10.88.1.1/32")})
			if err == nil {
				t.Fatal("failure ignored")
			}
			if io.calls[len(io.calls)-1] != "stop" {
				t.Fatal("forwarding not stopped", io.calls)
			}
			for _, s := range io.calls {
				if s == "send" {
					t.Fatal("advertised failed state")
				}
			}
			if err = r.Step(0, babel.Tick{}); !errors.Is(err, babel.ErrStopped) {
				t.Fatal("failed engine reused")
			}
		})
	}
}

func TestRoutingPersistsBeforeResumeAndSend(t *testing.T) {
	io := &routingRecorder{}
	r, _ := NewRouting(babel.Config{RouterID: 1}, nil, io)
	if err := r.Step(0, babel.Originate{Prefix: netip.MustParsePrefix("10.88.1.1/32")}); err != nil {
		t.Fatal(err)
	}
	if len(io.calls) != 3 || io.calls[0] != "apply" || io.calls[1] != "persist" || io.calls[2] != "resume" {
		t.Fatal(io.calls)
	}
	local, peer, _ := LinkAddresses(1, 2)
	io.calls = nil
	if err := r.Step(0, babel.AddLink{Link: babel.Link{ID: 1, Local: local, Peer: peer, Cost: 256}}); err != nil {
		t.Fatal(err)
	}
	persisted, resumed := false, false
	for _, call := range io.calls {
		switch call {
		case "persist":
			persisted = true
		case "resume":
			if !persisted {
				t.Fatal("resume before persistence")
			}
			resumed = true
		case "send":
			if !resumed {
				t.Fatal("advertise before resume")
			}
		}
	}
}

func TestAddressOwnershipAndScopedReuse(t *testing.T) {
	id, _ := RouterID(64, 445)
	v4, v6, _ := Addresses(64, 445)
	if v4.String() != "10.88.1.189" {
		t.Fatal(v4)
	}
	if !OwnedPrefix(64, id, netip.PrefixFrom(v4, 32)) || !OwnedPrefix(64, id, netip.PrefixFrom(v6, 128)) {
		t.Fatal("own prefix rejected")
	}
	if OwnedPrefix(65, id, netip.PrefixFrom(v4, 32)) || OwnedPrefix(64, id, netip.MustParsePrefix("0.0.0.0/0")) {
		t.Fatal("foreign prefix accepted")
	}
	a, b, _ := LinkAddresses(445, 460)
	c, d, _ := LinkAddresses(460, 445)
	if a != d || b != c || !a.IsLinkLocalUnicast() {
		t.Fatal("link-local mismatch")
	}
}
