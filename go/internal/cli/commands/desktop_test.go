package commands

import (
	"errors"
	"fmt"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
)

func TestDesktopPinRecoveryRequiresTypedRefusal(t *testing.T) {
	refusal := refuseDevicePin(devicePinDiagnostic{hostname: "ci-iq8.local", heading: "identity changed"})
	if got := DesktopPinRecovery(fmt.Errorf("connect: %w", refusal)); got != "ci-iq8.local" {
		t.Fatal(got)
	}
	mismatch := spkiRefusal("ci-iq8.local", &devicepin.PinMismatchError{Key: "urn:wendy:org:77:asset:537"})
	if got := DesktopPinRecovery(mismatch); got != "urn:wendy:org:77:asset:537" {
		t.Fatal(got)
	}
	for _, err := range []error{nil, errors.New(refusal.Error()), refuseIdentity("unrelated refusal"), errors.New("no route to host")} {
		if got := DesktopPinRecovery(err); got != "" {
			t.Fatalf("unexpected recovery: %q", got)
		}
	}
}

func TestDesktopUnpinClearsOnlySelectedDevice(t *testing.T) {
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"ci-iq8": {OrgID: 77, CloudGRPC: "grpc.a.sh:443", AssetID: "537"},
		"other":  {OrgID: 77, CloudGRPC: "grpc.a.sh:443", AssetID: "538"},
	})
	seedSPKIPins(t, "urn:wendy:org:77:asset:537", "urn:wendy:org:77:asset:538")
	if err := DesktopUnpin("ci-iq8.local"); err != nil {
		t.Fatal(err)
	}
	if _, ok := readPins()["ci-iq8"]; ok {
		t.Fatal("selected pin survived")
	}
	if _, ok := readPins()["other"]; !ok {
		t.Fatal("unrelated pin removed")
	}
	keys := readSPKIPins(t)
	if _, ok := keys["urn:wendy:org:77:asset:537"]; ok {
		t.Fatal("selected key survived")
	}
	if _, ok := keys["urn:wendy:org:77:asset:538"]; !ok {
		t.Fatal("unrelated key removed")
	}
}
