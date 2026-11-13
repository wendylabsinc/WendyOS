package commands

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// useEqualHostRoutes makes every candidate look equally routed from this host,
// as when one host interface reaches both of a device's answers.
func useEqualHostRoutes(t *testing.T) {
	t.Helper()
	origPref, origRoute, origIface := dialCandidateRoutePreferenceFn, routeInterfaceForIPFn, networkInterfaceRoutePreferenceFn
	t.Cleanup(func() {
		dialCandidateRoutePreferenceFn, routeInterfaceForIPFn, networkInterfaceRoutePreferenceFn = origPref, origRoute, origIface
	})
	dialCandidateRoutePreferenceFn = func(string) routePreference { return routeUnknown }
	routeInterfaceForIPFn = func(string) string { return "" }
	networkInterfaceRoutePreferenceFn = func(string) routePreference { return routeUnknown }
}

func useAddrOnLink(t *testing.T, addrs ...string) {
	t.Helper()
	orig := addrOnLinkFn
	t.Cleanup(func() { addrOnLinkFn = orig })
	addrOnLinkFn = func(addr string) bool { return slices.Contains(addrs, addr) }
}

func useResolver(t *testing.T, answers []string, browsed []models.LANDevice) {
	t.Helper()
	origLookup, origBrowse := osLookupHostFn, lanBrowseFn
	t.Cleanup(func() { osLookupHostFn, lanBrowseFn = origLookup, origBrowse })
	osLookupHostFn = func(context.Context, string) ([]string, error) {
		if len(answers) == 0 {
			return nil, errors.New("no answer")
		}
		return answers, nil
	}
	lanBrowseFn = func(context.Context, time.Duration) ([]models.LANDevice, error) { return browsed, nil }
}

func TestOrderRoutedDialCandidatesPutsTheDevicesCableFirst(t *testing.T) {
	useEqualHostRoutes(t)

	got := orderRoutedDialCandidates([]string{"192.168.2.216", "192.168.2.50"}, []string{"192.168.2.50"})

	if want := []string{"192.168.2.50", "192.168.2.216"}; !slices.Equal(got, want) {
		t.Errorf("orderRoutedDialCandidates = %v, want %v", got, want)
	}
}

// The device's cable only breaks ties: a host-side wired route still wins.
func TestOrderRoutedDialCandidatesKeepsTheHostRouteFirst(t *testing.T) {
	original := dialCandidateRoutePreferenceFn
	t.Cleanup(func() { dialCandidateRoutePreferenceFn = original })
	dialCandidateRoutePreferenceFn = func(ip string) routePreference {
		if ip == "10.42.0.9" {
			return routeWired
		}
		return routeWireless
	}

	got := orderRoutedDialCandidates([]string{"192.168.2.50", "10.42.0.9"}, []string{"192.168.2.50"})

	if want := []string{"10.42.0.9", "192.168.2.50"}; !slices.Equal(got, want) {
		t.Errorf("orderRoutedDialCandidates = %v, want %v", got, want)
	}
}

// Connecting by name after a discover: the resolver returns both answers and
// the cached record says which is the cable.
func TestResolveHostRanksTheCachedCableAddressFirst(t *testing.T) {
	useEqualHostRoutes(t)
	useAddrOnLink(t, "192.168.2.50")
	useCableAnswers(t)
	useResolver(t, []string{"192.168.2.216", "192.168.2.50"}, nil)
	setPinCache(t, discoverycache.Entry{ID: "pi", DisplayName: "pi", Hostname: "pi.local", IP: "192.168.2.216", WiredAddresses: []string{"192.168.2.50"}})

	got := resolveHostAllMDNSFallback(context.Background(), "pi.local")

	if want := []string{"192.168.2.50", "192.168.2.216"}; !slices.Equal(got, want) {
		t.Errorf("resolveHostAllMDNSFallback = %v, want %v", got, want)
	}
}

// A cable the resolver did not return joins only once it answers as the
// device; a cached record may be stale, or name another network's host.
func TestResolveHostAddsACachedCableOnlyWhenItAnswers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		answering []string
		want      []string
	}{
		{"cable answers", []string{"192.168.2.50"}, []string{"192.168.2.50", "192.168.2.216"}},
		{"cable silent", nil, []string{"192.168.2.216"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useEqualHostRoutes(t)
			useAddrOnLink(t, "192.168.2.50")
			useCableAnswers(t, tc.answering...)
			useResolver(t, []string{"192.168.2.216"}, nil)
			setPinCache(t, discoverycache.Entry{ID: "pi", DisplayName: "pi", Hostname: "pi.local", IP: "192.168.2.216", WiredAddresses: []string{"192.168.2.50"}})

			if got := resolveHostAllMDNSFallback(context.Background(), "pi.local"); !slices.Equal(got, tc.want) {
				t.Errorf("resolveHostAllMDNSFallback = %v, want %v", got, tc.want)
			}
		})
	}
}

// One host link hearing both of a device's addresses often caches just one;
// the browse then reports Wi-Fi alone, and the cable joins by answering.
func TestResolveHostAddsABrowsedCableThatAnswers(t *testing.T) {
	useEqualHostRoutes(t)
	useAddrOnLink(t, "192.168.2.50")
	useCableAnswers(t, "192.168.2.50")
	useResolver(t, nil, []models.LANDevice{{Hostname: "pi.local", IPAddress: "192.168.2.216", WiredAddresses: []string{"192.168.2.50"}}})
	setPinCache(t)

	got := resolveHostAllMDNSFallback(context.Background(), "pi.local")

	if want := []string{"192.168.2.50", "192.168.2.216"}; !slices.Equal(got, want) {
		t.Errorf("resolveHostAllMDNSFallback = %v, want %v", got, want)
	}
}

// A record alone adds nothing: a subnet match is no proof of a path or of the
// device.
func TestResolveHostNeverAddsASilentCable(t *testing.T) {
	useEqualHostRoutes(t)
	useAddrOnLink(t, "192.168.2.50")
	useCableAnswers(t)
	useResolver(t, nil, []models.LANDevice{{Hostname: "pi.local", IPAddress: "192.168.2.216", WiredAddresses: []string{"192.168.2.50"}}})
	setPinCache(t)

	got := resolveHostAllMDNSFallback(context.Background(), "pi.local")

	if want := []string{"192.168.2.216"}; !slices.Equal(got, want) {
		t.Errorf("resolveHostAllMDNSFallback = %v, want %v", got, want)
	}
}

// Without nss-mdns the browse is the only resolver; its record ranks the
// cable among the answers, which needs no question.
func TestResolveHostRanksTheBrowsedCableFirst(t *testing.T) {
	useEqualHostRoutes(t)
	useAddrOnLink(t, "192.168.2.50")
	asked := useCableAnswers(t)
	useResolver(t, nil, []models.LANDevice{
		{Hostname: "pi.local", IPAddress: "192.168.2.216", WiredAddresses: []string{"192.168.2.50"}},
		{Hostname: "pi.local", IPAddress: "192.168.2.50", WiredAddresses: []string{"192.168.2.50"}},
	})
	setPinCache(t)

	got := resolveHostAllMDNSFallback(context.Background(), "pi.local")

	if want := []string{"192.168.2.50", "192.168.2.216"}; !slices.Equal(got, want) || len(*asked) > 0 {
		t.Errorf("resolveHostAllMDNSFallback = %v after asking %v, want %v unasked", got, *asked, want)
	}
}

// Each cable is asked once, and only on this host's links.
func TestAnsweringCablesAsksEachLinkedCableOnce(t *testing.T) {
	useAddrOnLink(t, "192.168.2.50", "192.168.2.51")
	asked := useCableAnswers(t, "192.168.2.51")

	got := answeringCables(context.Background(), "pi.local", []string{"192.168.2.50", "10.9.0.4", "192.168.2.50", "192.168.2.51", "192.168.2.216"}, []string{"192.168.2.216"})

	if !slices.Equal(got, []string{"192.168.2.51"}) || !slices.Equal(*asked, []string{"192.168.2.50", "192.168.2.51"}) {
		t.Errorf("answeringCables = %v after asking %v", got, *asked)
	}
}

// useCableAnswers makes exactly the given addresses answer mDNS as pi.local,
// and records every address asked.
func useCableAnswers(t *testing.T, answering ...string) *[]string {
	t.Helper()
	var asked []string
	orig := cableAnswersFn
	t.Cleanup(func() { cableAnswersFn = orig })
	cableAnswersFn = func(_ context.Context, addr, host string) bool {
		asked = append(asked, addr)
		return host == "pi.local" && slices.Contains(answering, addr)
	}
	return &asked
}

// A Wi-Fi address in the cache must not keep the fast path off a live cable.
func TestPreferLiveCableReturnsToTheCable(t *testing.T) {
	useEqualHostRoutes(t)
	useAddrOnLink(t, "192.168.2.50")
	useCableAnswers(t, "192.168.2.50")

	got := preferLiveCable(context.Background(), discoverycache.Entry{Hostname: "pi.local", IP: "192.168.2.216", WiredAddresses: []string{"192.168.2.50"}})

	if got.IP != "192.168.2.50" {
		t.Errorf("IP = %s, want the live cable 192.168.2.50", got.IP)
	}
}

func TestPreferLiveCableKeepsTheCachedAddress(t *testing.T) {
	tests := []struct {
		name      string
		onLink    []string
		answering []string
		entry     discoverycache.Entry
		wantAsk   bool
	}{
		{"cable silent", []string{"192.168.2.50"}, nil, discoverycache.Entry{Hostname: "pi.local", IP: "192.168.2.216", WiredAddresses: []string{"192.168.2.50"}}, true},
		// Another device of the fleet holding the same address answers as itself.
		{"cable is another device", []string{"192.168.2.50"}, []string{"192.168.2.50"}, discoverycache.Entry{Hostname: "pi-2.local", IP: "192.168.2.216", WiredAddresses: []string{"192.168.2.50"}}, true},
		{"cable off this host's subnets", nil, []string{"10.9.0.4"}, discoverycache.Entry{Hostname: "pi.local", IP: "192.168.2.216", WiredAddresses: []string{"10.9.0.4"}}, false},
		{"already on the cable", []string{"192.168.2.50"}, nil, discoverycache.Entry{Hostname: "pi.local", IP: "192.168.2.50", WiredAddresses: []string{"192.168.2.50"}}, false},
		{"device without the record", nil, nil, discoverycache.Entry{Hostname: "pi.local", IP: "192.168.2.216"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useEqualHostRoutes(t)
			useAddrOnLink(t, tt.onLink...)
			asked := useCableAnswers(t, tt.answering...)

			if got := preferLiveCable(context.Background(), tt.entry); got.IP != tt.entry.IP {
				t.Errorf("IP = %s, want the cached %s", got.IP, tt.entry.IP)
			}
			if askedAny := len(*asked) > 0; askedAny != tt.wantAsk {
				t.Errorf("asked %v, want a question: %v", *asked, tt.wantAsk)
			}
		})
	}
}

// A cable that stays silent leaves the cache, so the next connect does not
// wait on it again; a cancelled connect proves nothing and forgets nothing.
func TestPreferLiveCableForgetsASilentCable(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want []string
	}{
		{"silent", context.Background(), nil},
		{"cancelled", cancelled, []string{"192.168.2.50"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useEqualHostRoutes(t)
			useAddrOnLink(t, "192.168.2.50")
			useCableAnswers(t)
			entry := discoverycache.Entry{ID: "pi", DisplayName: "pi", Hostname: "pi.local", IP: "192.168.2.216", WiredAddresses: []string{"192.168.2.50"}}
			setPinCache(t, entry)
			before, _ := cachedDeviceHostEntry("pi.local")

			preferLiveCable(tc.ctx, entry)

			after, _ := cachedDeviceHostEntry("pi.local")
			if !slices.Equal(after.WiredAddresses, tc.want) || !after.LastSeen.Equal(before.LastSeen) {
				t.Errorf("cached %v seen %v, want %v seen %v", after.WiredAddresses, after.LastSeen, tc.want, before.LastSeen)
			}
		})
	}
}

// A cached address this host reaches over its wire outranks a cable it would
// reach over Wi-Fi.
func TestPreferLiveCableKeepsABetterRoutedAddress(t *testing.T) {
	original := dialCandidateRoutePreferenceFn
	t.Cleanup(func() { dialCandidateRoutePreferenceFn = original })
	dialCandidateRoutePreferenceFn = func(ip string) routePreference {
		if ip == "10.42.0.120" {
			return routeWired
		}
		return routeWireless
	}
	useAddrOnLink(t, "192.168.2.50")
	asked := useCableAnswers(t, "192.168.2.50")

	got := preferLiveCable(context.Background(), discoverycache.Entry{Hostname: "pi.local", IP: "10.42.0.120", WiredAddresses: []string{"192.168.2.50"}})

	if got.IP != "10.42.0.120" || len(*asked) > 0 {
		t.Errorf("IP = %s after asking %v, want the better-routed address kept unasked", got.IP, *asked)
	}
}

// Discovery ranks a USB link above everything; the fast path keeps it.
func TestPreferLiveCableKeepsAUSBLink(t *testing.T) {
	useEqualHostRoutes(t)
	useAddrOnLink(t, "192.168.2.50")
	asked := useCableAnswers(t, "192.168.2.50")

	got := preferLiveCable(context.Background(), discoverycache.Entry{Hostname: "pi.local", IP: "169.254.198.132", WiredAddresses: []string{"192.168.2.50"}})

	if got.IP != "169.254.198.132" || len(*asked) > 0 {
		t.Errorf("IP = %s after asking %v, want the USB link kept unasked", got.IP, *asked)
	}
}

func TestLANDialCandidatesTriesALiveCableFirst(t *testing.T) {
	useEqualHostRoutes(t)
	useAddrOnLink(t, "192.168.2.50")
	useCableAnswers(t, "192.168.2.50")
	dev := models.LANDevice{Hostname: "pi.local", IPAddress: "192.168.2.216", Port: 50051, WiredAddresses: []string{"192.168.2.50"}}

	got := lanDialCandidates(context.Background(), dev)

	if len(got) == 0 || got[0] != "192.168.2.50:50051" || !slices.Contains(got, "192.168.2.216:50051") {
		t.Errorf("lanDialCandidates = %v, want the cable first and the found address kept", got)
	}
}

// A cable on this host's subnet may still be another network's address; one
// that does not answer must not take the found address's place.
func TestLANDialCandidatesKeepsTheFoundAddressWhenTheCableIsSilent(t *testing.T) {
	useEqualHostRoutes(t)
	useAddrOnLink(t, "192.168.2.50")
	useCableAnswers(t)
	dev := models.LANDevice{
		Hostname: "pi.local", IPAddress: "192.168.2.216", Port: 50051,
		Addresses:      []string{"192.168.2.216"},
		WiredAddresses: []string{"192.168.2.50"},
	}

	got := lanDialCandidates(context.Background(), dev)

	if len(got) == 0 || got[0] != "192.168.2.216:50051" || slices.Contains(got, "192.168.2.50:50051") {
		t.Errorf("lanDialCandidates = %v, want the found address first and no silent cable", got)
	}
}

func TestLANDialCandidatesKeepsAUSBLinkFirst(t *testing.T) {
	useEqualHostRoutes(t)
	useAddrOnLink(t, "192.168.2.50")
	useCableAnswers(t, "192.168.2.50")
	dev := models.LANDevice{
		Hostname: "pi.local", IPAddress: "169.254.198.132", Port: 50051, USB: "enx1",
		Addresses:      []string{"169.254.198.132", "192.168.2.50"},
		WiredAddresses: []string{"192.168.2.50"},
	}

	if got := lanDialCandidates(context.Background(), dev); len(got) < 2 || got[1] != "169.254.198.132:50051" {
		t.Errorf("lanDialCandidates = %v, want the USB rows first", got)
	}
}

// The cable only wins when this host reaches it at least as well.
func TestLANDialCandidatesKeepsABetterRoutedAddress(t *testing.T) {
	original := dialCandidateRoutePreferenceFn
	t.Cleanup(func() { dialCandidateRoutePreferenceFn = original })
	dialCandidateRoutePreferenceFn = func(ip string) routePreference {
		if ip == "192.168.2.216" {
			return routeWired
		}
		return routeWireless
	}
	dev := models.LANDevice{
		Hostname: "pi.local", IPAddress: "192.168.2.216", Port: 50051,
		Addresses:      []string{"192.168.2.216"},
		WiredAddresses: []string{"10.42.0.120"},
	}
	useAddrOnLink(t, "10.42.0.120")
	useCableAnswers(t, "10.42.0.120")

	if got := lanDialCandidates(context.Background(), dev); len(got) == 0 || got[0] != "192.168.2.216:50051" {
		t.Errorf("lanDialCandidates = %v, want the better-routed 192.168.2.216 first", got)
	}
}
