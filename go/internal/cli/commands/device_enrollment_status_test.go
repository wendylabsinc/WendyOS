package commands

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

type statusReaderStub struct{ statusOnly bool }

func (s *statusReaderStub) GetDeviceIdentity(time.Duration) (*liteclient.DeviceIdentity, error) {
	return &liteclient.DeviceIdentity{ID: "hardware", Name: "Camera"}, nil
}
func (s *statusReaderStub) EnrollmentChallenge(statusOnly bool) (*litepb.WendyComEnrollmentChallenge, error) {
	s.statusOnly = statusOnly
	return &litepb.WendyComEnrollmentChallenge{Enrolled: true, DeviceId: "cloud-device", TenantId: "tenant", NonceHex: "private-nonce"}, nil
}
func TestLiteEnrollmentStatusIsReadOnlyAndSeparatesIdentities(t *testing.T) {
	reader := &statusReaderStub{}
	result, err := readLiteEnrollmentStatus(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !reader.statusOnly || !result.Enrolled || result.HardwareID != "hardware" || result.DeviceID != "cloud-device" {
		t.Fatalf("wrong status: %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "nonce") {
		t.Fatal("status leaked the enrollment nonce")
	}
}
