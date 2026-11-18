package commands

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestDesktopLocalAddressCandidates(t *testing.T) {
	onLink := func(ip string) bool { return ip == "192.168.1.42" || ip == "fd00::42" }
	lookup := func(zone string) (*net.Interface, error) {
		switch zone {
		case "en7", "7":
			return &net.Interface{Name: "en7", Index: 7, Flags: net.FlagUp}, nil
		case "down":
			return &net.Interface{Name: "down"}, nil
		case "lo0":
			return &net.Interface{Name: "lo0", Flags: net.FlagUp | net.FlagLoopback}, nil
		default:
			return nil, errors.New("no interface")
		}
	}
	for _, tc := range []struct {
		name, ip, iface string
		port            int
		want            []string
	}{
		{"legacy advertised port", "192.168.1.42", "", 50051, []string{"192.168.1.42:50051", "192.168.1.42:50052"}},
		{"custom advertised port", "192.168.1.42", "", 50100, []string{"192.168.1.42:50100", "192.168.1.42:50101"}},
		{"unknown port", "192.168.1.42", "", 0, []string{"192.168.1.42:50052"}},
		{"maximum port", "192.168.1.42", "", 65535, []string{"192.168.1.42:65535"}},
		{"invalid port", "192.168.1.42", "", 65536, nil},
		{"routable IPv6", "fd00::42", "", 0, []string{"[fd00::42]:50052"}},
		{"IPv6 local scope", "fe80::42", "en7", 0, []string{"[fe80::42%en7]:50052"}},
		{"existing IPv6 zone", "fe80::42%en7", "en7", 0, []string{"[fe80::42%en7]:50052"}},
		{"numeric IPv6 zone", "fe80::42%7", "", 0, []string{"[fe80::42%7]:50052"}},
		{"IPv4 local scope", "169.254.10.42", "en7", 0, []string{"169.254.10.42:50052"}},
		{"missing scope", "fe80::42", "", 0, nil},
		{"mismatched scope", "fe80::42%en7", "other", 0, nil},
		{"removed interface", "fe80::42", "removed", 0, nil},
		{"down interface", "fe80::42", "down", 0, nil},
		{"loopback interface", "fe80::42", "lo0", 0, nil},
		{"routed remote", "192.168.2.42", "", 0, nil},
		{"public remote", "203.0.113.42", "", 0, nil},
		{"loopback", "127.0.0.1", "", 0, nil},
		{"multicast", "ff02::1", "en7", 0, nil},
		{"hostname", "camera.local", "", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := desktopLocalAddressCandidates(tc.ip, tc.port, tc.iface, onLink, lookup); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("candidates = %v, want %v", got, tc.want)
			}
		})
	}
}

type localRaceCloser struct {
	closed chan struct{}
	count  atomic.Int32
}

func (c *localRaceCloser) Close() error {
	if c.count.Add(1) == 1 {
		close(c.closed)
	}
	return nil
}

func TestDesktopLocalRaceReturnsLiveAddressBeforeStaleDialFinishes(t *testing.T) {
	previous := desktopLocalDial
	t.Cleanup(func() { desktopLocalDial = previous })
	started, canceled, finishSlow := make(chan struct{}), make(chan struct{}), make(chan struct{})
	loser := &localRaceCloser{closed: make(chan struct{})}
	winner := &grpcclient.AgentConnection{}
	desktopLocalDial = func(ctx context.Context, address string, _ config.CertificateInfo, _ certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		if address == "stale" {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-finishSlow
			// Even a dial that returns success after cancellation must close.
			return &grpcclient.AgentConnection{ExtraClosers: []io.Closer{loser}}, nil
		}
		<-started
		return winner, nil
	}
	result := make(chan *grpcclient.AgentConnection, 1)
	go func() {
		result <- raceDesktopLocal(context.Background(), []string{"stale", "live"}, config.CertificateInfo{}, certs.WendyIdentity{})
	}()
	select {
	case got := <-result:
		if got != winner {
			t.Fatal("did not select live candidate")
		}
	case <-time.After(time.Second):
		close(finishSlow)
		t.Fatal("stale candidate blocked live candidate")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("losing dial was not cancelled")
	}
	close(finishSlow)
	select {
	case <-loser.closed:
	case <-time.After(time.Second):
		t.Fatal("late successful loser leaked")
	}
	if loser.count.Load() != 1 {
		t.Fatal("loser closed more than once")
	}
	winner.Close()
}

func TestDesktopLocalRaceBoundsConcurrencyAndHonorsCancellation(t *testing.T) {
	previous := desktopLocalDial
	t.Cleanup(func() { desktopLocalDial = previous })
	entered := make(chan struct{}, 16)
	finished := make(chan struct{}, 16)
	var calls atomic.Int32
	desktopLocalDial = func(ctx context.Context, _ string, _ config.CertificateInfo, _ certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-ctx.Done()
		finished <- struct{}{}
		return nil, ctx.Err()
	}
	var candidates []string
	for n := range 16 {
		candidates = append(candidates, strconv.Itoa(n))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan *grpcclient.AgentConnection, 1)
	go func() { result <- raceDesktopLocal(ctx, candidates, config.CertificateInfo{}, certs.WendyIdentity{}) }()
	for range desktopLocalCandidateLimit {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("candidate did not start")
		}
	}
	cancel()
	select {
	case conn := <-result:
		if conn != nil {
			t.Fatal("cancelled race returned connection")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked")
	}
	for range desktopLocalCandidateLimit {
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("dial retained cancelled context")
		}
	}
	if calls.Load() != desktopLocalCandidateLimit {
		t.Fatalf("started %d candidates", calls.Load())
	}
}

func TestDesktopLocalRaceClosesFailedDialConnection(t *testing.T) {
	previous := desktopLocalDial
	t.Cleanup(func() { desktopLocalDial = previous })
	closer := &localRaceCloser{closed: make(chan struct{})}
	desktopLocalDial = func(context.Context, string, config.CertificateInfo, certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{ExtraClosers: []io.Closer{closer}}, errors.New("probe failed")
	}
	if conn := raceDesktopLocal(context.Background(), []string{"failed"}, config.CertificateInfo{}, certs.WendyIdentity{}); conn != nil {
		t.Fatal("failed probe accepted")
	}
	select {
	case <-closer.closed:
	default:
		t.Fatal("failed dial leaked connection")
	}
}
