package commands

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// A binding saved by an older CLI names an organization but no device. No
// certificate can satisfy it, so every connection is refused until it is
// cleared, and the refusal has to say which binding and how to clear it. The
// bare identity error named neither (WendyOS#2936).
func TestOrgOnlyBindingRefusalNamesUnpin(t *testing.T) {
	restoreDeviceGlobals(t)
	cert := selfSignedCLICert(t, 2)
	cfg := &config.Config{Auth: []config.AuthConfig{{Certificates: []config.CertificateInfo{cert}}}}
	cfg.SetDevicePin("old-robot.local", 2, "cloud.test:443", "", "")
	setTempConfig(t, cfg)
	setPinCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Nothing listens here: the refusal is decided before any dial.
	conn, _, err := dialAgentLadderWithCerts(ctx, newDialTarget("old-robot.local", "127.0.0.1:1"), []config.CertificateInfo{cert})
	if conn != nil {
		conn.Close()
		t.Fatal("connected to a device no saved identity could verify")
	}
	// Recovery, Cloud name fallback and discovery all stop on this sentinel.
	if !errors.Is(err, certs.ErrExpectedDeviceIdentity) {
		t.Fatalf("refusal %v is no longer a missing expected identity", err)
	}
	if !strings.Contains(err.Error(), `"old-robot.local"`) {
		t.Fatalf("refusal does not name the saved binding: %v", err)
	}
	want := []string{"Clear the saved identity, then connect again: wendy device unpin old-robot.local"}
	if got := NextSteps(err); !reflect.DeepEqual(got, want) {
		t.Fatalf("next steps = %q, want %q", got, want)
	}
}

// Discovery meets the same refusal for a device it finds. Its row has to give
// the same recovery: retrying with TLS debugging, the generic advice, shows
// nothing, because no connection is ever attempted.
func TestDiscoveryExplainsOrgOnlyBinding(t *testing.T) {
	dev := models.LANDevice{Hostname: "old-robot.local", DisplayName: "old-robot"}
	failure := lanProbeFailure(dev, errors.Join(orgOnlyBindingError{key: "old-robot.local"}, errors.New("rpc error: code = Unavailable")), certsForOrgs(1))
	if failure == nil || !strings.Contains(failure.Message, "names an organization but no device") {
		t.Fatalf("discovery did not explain the saved binding: %+v", failure)
	}
	want := []string{"Clear the saved identity, then connect again: wendy device unpin old-robot.local"}
	if !reflect.DeepEqual(failure.NextSteps, want) {
		t.Fatalf("next steps = %q, want %q", failure.NextSteps, want)
	}
	// A name with terminal controls is never pasted into a command.
	failure = lanProbeFailure(dev, orgOnlyBindingError{key: "old\x1b[2Jrobot.local"}, certsForOrgs(1))
	if failure == nil || len(failure.NextSteps) != 1 || strings.Contains(failure.NextSteps[0], "\x1b") || strings.Contains(failure.Message, "\x1b") {
		t.Fatalf("discovery rendered an unsafe binding name: %+v", failure)
	}
}
