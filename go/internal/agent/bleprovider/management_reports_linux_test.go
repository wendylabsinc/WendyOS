//go:build linux

package bleprovider

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"golang.org/x/sys/unix"
)

func managementPacket(index uint16, kind byte, flags uint32, data []byte) []byte {
	p := make([]byte, 20+len(data))
	binary.LittleEndian.PutUint16(p[:2], mgmtDeviceFound)
	binary.LittleEndian.PutUint16(p[2:4], index)
	binary.LittleEndian.PutUint16(p[4:6], uint16(len(p)-6))
	copy(p[6:12], []byte{1, 0xee, 0xdd, 0xcc, 0xbb, 0xaa})
	p[12] = kind
	p[13] = 200 // -56 dBm
	binary.LittleEndian.PutUint32(p[14:18], flags)
	binary.LittleEndian.PutUint16(p[18:20], uint16(len(data)))
	copy(p[20:], data)
	return p
}

func TestManagementReportEnvelope(t *testing.T) {
	u, n := nameFields(lowDevice, "field")
	packet := managementPacket(2, btAddrLERandom, 1<<5, append(u, n...))
	got, matched, err := decodeManagementAdvertisement(packet, 2)
	if err != nil || !matched || got.address != "AA:BB:CC:DD:EE:01" || got.addressType != "random" || !got.scanResponse || !bytes.Equal(got.data, packet[20:]) {
		t.Fatalf("decoded %+v %v %v", got, matched, err)
	}
	if _, matched, err := decodeManagementAdvertisement(packet, 1); err != nil || matched {
		t.Fatal("other adapter accepted")
	}
	classic := bytes.Clone(packet)
	classic[12] = 0
	if _, matched, err := decodeManagementAdvertisement(classic, 2); err != nil || matched {
		t.Fatal("BR/EDR accepted")
	}
	for _, bad := range [][]byte{nil, packet[:5], packet[:19], append(bytes.Clone(packet), 0), func() []byte { p := bytes.Clone(packet); p[18]++; return p }()} {
		if _, _, err := decodeManagementAdvertisement(bad, 2); err == nil {
			t.Fatalf("accepted malformed envelope %x", bad)
		}
	}
}

// A UNIX packet socket exercises the real nonblocking receiver and cancellation
// without a Bluetooth adapter, management privileges or any hardware access.
func TestManagementReceiverUnixPacketIntegration(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[1])
	source := &managementReportSocket{fd: fds[0]}
	f := newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	status := make(chan error, 8)
	go func() {
		done <- runManagementReports(ctx, 0, f, source, func() (managementReportReader, error) { return nil, errors.New("unexpected reopen") }, status)
	}()
	u, n := nameFields(lowDevice, "field")
	packets := [][]byte{managementPacket(1, btAddrLEPublic, 0, append(bytes.Clone(u), n...)), {1, 2, 3}, managementPacket(0, btAddrLEPublic, 0, u), managementPacket(0, btAddrLEPublic, 1<<5, n)}
	for _, p := range packets {
		if _, err := unix.Write(fds[1], p); err != nil {
			t.Fatal(err)
		}
	}
	_, objects := reportObjects("AA_BB_CC_DD_EE_01")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if got, _ := f.candidates(objects, time.Now()); len(got) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("live receiver did not correlate reports")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("receiver did not cancel")
	}
	// Close is idempotent, including Run's setup-failure cleanup ownership.
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagementReceiverReportsInvalidDescriptor(t *testing.T) {
	// POLLNVAL must become a source failure, rather than a busy poll loop.
	source := &managementReportSocket{fd: -1}
	// poll ignores negative descriptors, so use a valid but already closed fd.
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	source.fd = fds[0]
	_ = unix.Close(fds[0])
	defer unix.Close(fds[1])
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := source.Read(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("invalid fd: %v", err)
	}
}

func TestUnprivilegedManagementSourceFailsExplicitly(t *testing.T) {
	var capabilities [2]unix.CapUserData
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	if err := unix.Capget(&header, &capabilities[0]); err != nil {
		t.Fatal(err)
	}
	if capabilities[0].Effective&(uint32(1)<<unix.CAP_NET_ADMIN) != 0 {
		t.Skip("run in the separate unprivileged container check")
	}
	reader, err := openManagementReports()
	if reader != nil {
		reader.Close()
		t.Fatal("opened a source without its required capability")
	}
	if !errors.Is(err, unix.EPERM) {
		t.Fatalf("missing capability was not explicit: %v", err)
	}
}

type scriptedManagementReader struct {
	read   func(context.Context) ([]byte, error)
	closed atomic.Bool
}

func (s *scriptedManagementReader) Read(ctx context.Context) ([]byte, error) { return s.read(ctx) }
func (s *scriptedManagementReader) Close() error                             { s.closed.Store(true); return nil }

func TestManagementSourceFailureRecoveryClearsEpoch(t *testing.T) {
	f := newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture)
	u, n := nameFields(lowDevice, "field")
	f.observeReport("AA:BB:CC:DD:EE:01", "public", append(bytes.Clone(u), n...), time.Now())
	first := &scriptedManagementReader{read: func(context.Context) ([]byte, error) { return nil, io.ErrUnexpectedEOF }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second := &scriptedManagementReader{read: func(ctx context.Context) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }}
	status := make(chan error, 8)
	done := make(chan error, 1)
	go func() {
		done <- runManagementReports(ctx, 0, f, first, func() (managementReportReader, error) { return second, nil }, status)
	}()
	select {
	case err := <-status:
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("failure not reported")
	}
	_, objects := reportObjects("AA_BB_CC_DD_EE_01")
	if got, _ := f.candidates(objects, time.Now()); len(got) != 0 {
		t.Fatal("failed source retained name observation")
	}
	select {
	case err := <-status:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("recovery not reported")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recovery loop stuck")
	}
	if !first.closed.Load() || !second.closed.Load() {
		t.Fatal("source leaked")
	}
}

func TestManagementOpenFailureCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture)
	want := errors.New("permission denied")
	status := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- runManagementReports(ctx, 0, f, nil, func() (managementReportReader, error) { return nil, want }, status)
	}()
	select {
	case err := <-status:
		if !errors.Is(err, want) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("open failure not reported")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retry sleep ignored cancellation")
	}
}

func TestDiscoveryHealthKeepsIndependentSourceFailures(t *testing.T) {
	var statuses []localmesh.CarrierStatus
	r := &runtime{cfg: Config{Status: func(s localmesh.CarrierStatus) { statuses = append(statuses, s) }}}
	ctx := context.Background()
	scanErr := errors.New("scan failed")
	reportErr := errors.New("report failed")
	r.reportErr = reportErr
	r.publishDiscoveryHealth(ctx)
	r.reportDiscoveryHealth(ctx, scanErr)
	r.reportErr = nil
	r.publishDiscoveryHealth(ctx)
	if !errors.Is(r.discoveryErr, scanErr) {
		t.Fatal("report recovery cleared scan failure")
	}
	r.reportErr = reportErr
	r.reportDiscoveryHealth(ctx, nil)
	if !errors.Is(r.discoveryErr, reportErr) {
		t.Fatal("scan success cleared report failure")
	}
	r.reportErr = nil
	r.publishDiscoveryHealth(ctx)
	if r.discoveryErr != nil || len(statuses) != 5 || !statuses[4].Ready || statuses[4].Err != nil {
		t.Fatalf("statuses=%+v", statuses)
	}
}

func FuzzManagementAdvertisement(f *testing.F) {
	f.Add(managementPacket(0, btAddrLEPublic, 1<<5, []byte{2, 1, 6}))
	f.Fuzz(func(t *testing.T, packet []byte) {
		a, matched, err := decodeManagementAdvertisement(packet, 0)
		if err == nil && matched {
			_, _ = parseAdvertisingData(a.data, ServiceUUID(testTenant))
		}
	})
}

// Run publishes the setup result before starting the receiver. Its first reopen
// can succeed without an intervening failure event; recovery must still replace
// the initial degraded status.
func TestManagementImmediateOpenRecoveryClearsInitialHealth(t *testing.T) {
	initialErr := errors.New("initial report source unavailable")
	var statuses []localmesh.CarrierStatus
	r := &runtime{reportErr: initialErr, cfg: Config{Status: func(s localmesh.CarrierStatus) { statuses = append(statuses, s) }}}
	r.publishDiscoveryHealth(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &scriptedManagementReader{read: func(ctx context.Context) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }}
	status := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- runManagementReports(ctx, 0, newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture), nil,
			func() (managementReportReader, error) { return reader, nil }, status)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("recovered source did not stop")
		}
	}()
	select {
	case err := <-status:
		r.reportErr = err
		r.publishDiscoveryHealth(ctx)
	case <-time.After(time.Second):
		t.Fatal("immediate recovery not reported")
	}
	if len(statuses) != 2 || !statuses[1].Ready || statuses[1].Err != nil {
		t.Fatalf("initial source failure remained published after recovery: %+v", statuses)
	}
}

func TestDiscoveryPublishesInitialHealthyStatus(t *testing.T) {
	var statuses []localmesh.CarrierStatus
	r := &runtime{cfg: Config{Status: func(s localmesh.CarrierStatus) { statuses = append(statuses, s) }}}
	r.publishDiscoveryHealth(context.Background())
	if len(statuses) != 1 || !statuses[0].Ready || statuses[0].Err != nil {
		t.Fatalf("healthy setup was not published: %+v", statuses)
	}
}
