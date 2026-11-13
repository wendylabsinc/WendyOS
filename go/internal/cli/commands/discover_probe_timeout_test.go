package commands

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// `wendy discover --json` used to leave a slow Docker out of the list without
// a word. It is still left out (discovery must stay fast), but reported.
func TestDiscoverExternalDevicesReporting_ReportsRuntimesThatDidNotAnswer(t *testing.T) {
	slow := &fakeProvider{key: providers.ProviderKeyDocker, discoverErr: &providers.ProbeTimeoutError{Runtime: "Docker", After: 3 * time.Second}}
	broken := &fakeProvider{key: "wendy-lite", discoverErr: errors.New("serial port busy")}
	ok := &fakeProvider{key: "other", devices: []models.ExternalDevice{extDevice("board")}}
	withExternalProviders(t, slow, broken, ok)

	var skipped []error
	got := discoverExternalDevicesReporting(context.Background(), true, func(err error) { skipped = append(skipped, err) })
	if len(got) != 1 || got[0].DisplayName != "board" {
		t.Fatalf("devices = %v, want only the responsive provider's device", got)
	}
	if len(skipped) != 1 || skipped[0].Error() != "Docker did not answer within 3s" {
		t.Fatalf("skipped = %v, want only the timed-out runtime reported", skipped)
	}
}

// probeRecordingProvider records the probe bound of each DiscoverDevices call
// and answers from its script: a ProbeTimeoutError for that bound, or devices.
type probeRecordingProvider struct {
	*fakeProvider
	timeouts []time.Duration
	answerOn int // the call (1-based) that answers with devices; 0 = never
}

func (p *probeRecordingProvider) DiscoverDevices(ctx context.Context) ([]models.ExternalDevice, error) {
	d := providers.ProbeTimeout(ctx)
	p.timeouts = append(p.timeouts, d)
	if len(p.timeouts) == p.answerOn {
		return []models.ExternalDevice{extDevice("docker")}, nil
	}
	return nil, &providers.ProbeTimeoutError{Runtime: "Docker", After: d}
}

func newProbeRecordingProvider(answerOn int) *probeRecordingProvider {
	return &probeRecordingProvider{fakeProvider: &fakeProvider{key: providers.ProviderKeyDocker}, answerOn: answerOn}
}

// `--device docker` against a slow-but-healthy daemon used to fail with "no
// Docker devices found" after the 3 s discovery bound. With no one at the
// terminal it waits 10 s, then fails as device_unreachable naming the timeout.
func TestExplicitProviderDevice_NoPersonLongerBoundAndDistinctError(t *testing.T) {
	stubHumanPresent(t, false)
	p := newProbeRecordingProvider(0)

	_, err := explicitProviderDevice(context.Background(), p)
	if len(p.timeouts) != 1 || p.timeouts[0] != 10*time.Second {
		t.Fatalf("probe bounds = %v, want one 10s probe", p.timeouts)
	}
	if err == nil || !strings.Contains(err.Error(), "did not answer within 10s") || strings.Contains(err.Error(), "no Docker devices") {
		t.Fatalf("err = %v, want the timeout named, not \"no devices found\"", err)
	}
	if !errors.Is(err, errDeviceUnreachable) || ErrorClass(err) != "device_unreachable" {
		t.Fatalf("err class = %q, want device_unreachable", ErrorClass(err))
	}
}

// With a person at the terminal the runtime may be starting, so — like
// ensureDockerDaemon — it is given up to dockerDaemonReadyWait.
func TestExplicitProviderDevice_PersonPresentWaitsLikeEnsureDockerDaemon(t *testing.T) {
	stubHumanPresent(t, true)

	slow := newProbeRecordingProvider(2) // answers on the longer, second probe
	sel, err := explicitProviderDevice(context.Background(), slow)
	if err != nil || sel == nil || sel.External == nil || sel.Provider != slow {
		t.Fatalf("explicitProviderDevice = %+v, %v; want the slow runtime selected", sel, err)
	}
	if want := []time.Duration{10 * time.Second, dockerDaemonReadyWait}; len(slow.timeouts) != 2 || slow.timeouts[0] != want[0] || slow.timeouts[1] != want[1] {
		t.Fatalf("probe bounds = %v, want %v", slow.timeouts, want)
	}

	hung := newProbeRecordingProvider(0)
	_, err = explicitProviderDevice(context.Background(), hung)
	if err == nil || !strings.Contains(err.Error(), "did not answer within 1m0s") || !errors.Is(err, errDeviceUnreachable) {
		t.Fatalf("err = %v, want a device_unreachable error naming the 60s wait", err)
	}

	quick := newProbeRecordingProvider(1)
	if _, err := explicitProviderDevice(context.Background(), quick); err != nil || len(quick.timeouts) != 1 {
		t.Fatalf("a runtime that answers at once: err = %v, probes = %v; want one probe", err, quick.timeouts)
	}
}

// Text-mode `wendy discover --timeout …` also used to drop a slow runtime
// without a word; it now collects it during the scan (discoverOnce's TUI
// can't run without a terminal, so the scan is tested on its own) and prints
// the same stderr warning as --json.
func TestDiscoverOnceScan_ReportsRuntimesThatDidNotAnswer(t *testing.T) {
	slow := &fakeProvider{key: "wendy-lite", discoverErr: &providers.ProbeTimeoutError{Runtime: "Docker", After: 3 * time.Second}}
	ok := &fakeProvider{key: "other", devices: []models.ExternalDevice{extDevice("board")}}
	withExternalProviders(t, slow, ok)

	collection, skipped, err := discoverOnceScan(context.Background(), externalOpts(), true)
	if err != nil {
		t.Fatalf("discoverOnceScan: %v", err)
	}
	if collection == nil || len(collection.ExternalDevices) != 1 {
		t.Fatalf("collection = %+v, want the responsive provider's device", collection)
	}
	var buf bytes.Buffer
	warnSkippedRuntimes(&buf, skipped)
	if got, want := buf.String(), "Warning: Docker did not answer within 3s; it is not listed.\n"; got != want {
		t.Fatalf("warnings = %q, want %q", got, want)
	}
}
