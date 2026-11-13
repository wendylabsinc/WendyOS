package commands

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

var errBrowse = errors.New("dns-sd error -65563")

// localOnly is what `wendy discover --json` returns when the network scan saw
// nothing: just the always-present local run targets.
func localOnly() *models.DevicesCollection {
	return &models.DevicesCollection{ExternalDevices: []models.ExternalDevice{
		{ID: "local", ProviderKey: providers.ProviderKeyLocal},
		{ID: "docker", ProviderKey: providers.ProviderKeyDocker},
	}}
}

func TestLANBrowseOutcome_NoBrowseErrorIsSilent(t *testing.T) {
	warning, err := lanBrowseOutcome(localOnly(), nil)
	if warning != "" || err != nil {
		t.Fatalf("got (%q, %v), want nothing", warning, err)
	}
}

func TestLANBrowseOutcome_NothingFoundIsAnError(t *testing.T) {
	_, err := lanBrowseOutcome(localOnly(), errBrowse)
	if err == nil {
		t.Fatal("want an error when the browse failed and only local targets were listed")
	}
	for _, want := range []string{"dns-sd error -65563", "sandbox", "Local Network", "--device"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLANBrowseOutcome_DevicesFoundOnlyWarns(t *testing.T) {
	c := localOnly()
	c.LANDevices = []models.LANDevice{{Hostname: "wendyos-hopeful-glider.local"}}
	warning, err := lanBrowseOutcome(c, errBrowse)
	if err != nil {
		t.Fatalf("err = %v, want a warning only when devices were still found", err)
	}
	if !strings.Contains(warning, "dns-sd error -65563") {
		t.Fatalf("warning = %q, want it to carry the browse error", warning)
	}
}

func TestLANBrowseOutcome_NonLocalExternalDeviceCounts(t *testing.T) {
	c := localOnly()
	c.ExternalDevices = append(c.ExternalDevices, models.ExternalDevice{ID: "lite-1", ProviderKey: "wendy-lite"})
	if _, err := lanBrowseOutcome(c, errBrowse); err != nil {
		t.Fatalf("a Wendy Lite board was found; want a warning, got error %v", err)
	}
}

func TestLANBrowseErrors_KeepsFirstAndIsConcurrencySafe(t *testing.T) {
	var b lanBrowseErrors
	if b.first() != nil {
		t.Fatal("zero value must report no error")
	}
	b.record(errBrowse)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { b.record(errors.New("later")) })
	}
	wg.Wait()
	if !errors.Is(b.first(), errBrowse) {
		t.Fatalf("first() = %v, want the first recorded error", b.first())
	}
}
