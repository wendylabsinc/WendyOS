package backend

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
)

func TestVPNCloudOutagePreservesMeshAndRouteIntent(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const device = "22222222-2222-4222-8222-222222222222"
	const remote = "33333333-3333-4333-8333-333333333333"
	mesh := vpn.Route{Selector: "mesh://" + tenant + "/" + device, Name: "local"}
	cloud := vpn.Route{Selector: "cloud://api.wendy.dev/tenant/" + tenant + "/asset/" + remote, Name: "remote", Principal: "spiffe://wendy.sh/tenant/" + tenant + "/device/" + remote}
	input := []vpn.Route{mesh, cloud}
	failure := errors.New("Cloud inventory unavailable")
	offline, err := prepareVPNRoutes(context.Background(), input, func(ctx context.Context, selectors []string) (map[string]commands.DesktopCloudRouteIdentity, error) {
		if !reflect.DeepEqual(selectors, []string{cloud.Selector}) {
			t.Fatal(selectors)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("unbounded mixed-route inventory refresh")
		}
		return nil, failure
	})
	if err != nil || len(offline) != 2 || offline[0] != mesh || offline[1].Selector != cloud.Selector || offline[1].Principal != "" {
		t.Fatalf("offline routes=%+v error=%v", offline, err)
	}
	if input[1].Principal != cloud.Principal {
		t.Fatal("modified caller's saved route intent")
	}
	// Keep stable VIPs, while withdrawing all private Cloud aliases.
	before, err := vpn.BuildRoutes(input)
	if err != nil {
		t.Fatal(err)
	}
	after, err := vpn.BuildRoutes(offline)
	if err != nil {
		t.Fatal(err)
	}
	for i := range before {
		if before[i].Address != after[i].Address {
			t.Fatal("outage reassigned VIPs")
		}
	}
	answer := func(name string, routes []vpn.Route) *dns.Msg {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion(dns.Fqdn(name), dns.TypeA)
		wire, _ := q.Pack()
		wire, e := vpn.AnswerDNS(wire, routes)
		if e != nil {
			t.Fatal(e)
		}
		r := new(dns.Msg)
		if e = r.Unpack(wire); e != nil {
			t.Fatal(e)
		}
		return r
	}
	if r := answer("local.wendy.internal", after); len(r.Answer) != 1 {
		t.Fatal("mesh alias lost", r)
	}
	for _, name := range []string{"remote.wendy.internal", "device-" + remote + ".wendy.internal"} {
		if r := answer(name, after); len(r.Answer) != 0 {
			t.Fatal("unverified Cloud alias survived", r)
		}
	}
	for _, port := range []uint16{18080, 22, 50052} {
		if c, e := desktopRouteDial(after)(context.Background(), cloud.Selector, port); e == nil {
			c.Close()
			t.Fatal("unverified Cloud forwarding admitted")
		} else if !strings.Contains(e.Error(), "Cloud route identity is unavailable") {
			t.Fatal(e)
		}
	}
	// Re-preparing the retained selectors restores verified Cloud aliases.
	online, err := prepareVPNRoutes(context.Background(), offline, func(context.Context, []string) (map[string]commands.DesktopCloudRouteIdentity, error) {
		return map[string]commands.DesktopCloudRouteIdentity{cloud.Selector: {Name: cloud.Name, Principal: cloud.Principal}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	online, err = vpn.BuildRoutes(online)
	if err != nil {
		t.Fatal(err)
	}
	if r := answer("remote.wendy.internal", online); len(r.Answer) != 1 {
		t.Fatal("Cloud alias did not recover", r)
	}
	for i := range before {
		if before[i].Address != online[i].Address {
			t.Fatal("recovery reassigned VIPs")
		}
	}
	// Cloud-only failures and caller cancellation cannot turn into empty success.
	if _, err = prepareVPNRoutes(context.Background(), []vpn.Route{cloud}, func(context.Context, []string) (map[string]commands.DesktopCloudRouteIdentity, error) {
		return nil, failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if _, err = prepareVPNRoutes(ctx, input, func(context.Context, []string) (map[string]commands.DesktopCloudRouteIdentity, error) {
		cancel()
		return nil, failure
	}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestVPNFailedReplacementPreservesRunningRelay(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	b := New()
	defer b.StopVPN()
	cfg, err := b.StartVPN([]vpn.Route{{Selector: "mesh:local", Name: "local"}})
	if err != nil {
		t.Fatal(err)
	}
	previous := b.relay
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, route := range []vpn.Route{{Selector: "cloud://invalid"}, {Selector: "invalid"}} {
		if _, err = b.StartVPN([]vpn.Route{route}); err == nil {
			t.Fatal("invalid replacement accepted")
		}
		if b.relay != previous || !vpn.Healthy(ctx, cfg) {
			t.Fatal("failed replacement stopped the active relay")
		}
	}
	if _, err := b.StartVPN([]vpn.Route{{Selector: "mesh:local"}}, "invalid-gateway"); err == nil {
		t.Fatal("invalid gateway accepted")
	}
	if b.relay != previous || !vpn.Healthy(ctx, cfg) {
		t.Fatal("invalid gateway preparation stopped the active relay")
	}
	replacement, err := b.StartVPN([]vpn.Route{{Selector: "mesh:replacement"}})
	if err != nil {
		t.Fatal(err)
	}
	if vpn.Healthy(ctx, cfg) || !vpn.Healthy(ctx, replacement) {
		t.Fatal("successful replacement did not retire only the old relay")
	}
}
