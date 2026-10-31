package backend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type nearbyFake struct {
	closed, writes   int
	nonce            []bool
	loseReply        bool
	wendyOS          bool
	rejectEnrollment bool
	lastRequest      *pb.SetupEnrollCommand
}

func (f *nearbyFake) Status(_ context.Context, nonce bool) (*pb.SetupStatusResponse, error) {
	f.nonce = append(f.nonce, nonce)
	if f.wendyOS {
		return &pb.SetupStatusResponse{AcmeSupported: true}, nil
	}
	return &pb.SetupStatusResponse{DeviceClass: "C", LiteSupported: true}, nil
}
func (f *nearbyFake) Call(_ context.Context, c *pb.BluetoothCommand) (*pb.BluetoothResponse, error) {
	if c.GetWifiStatus() != nil {
		return &pb.BluetoothResponse{Response: &pb.BluetoothResponse_WifiStatus{WifiStatus: &pb.WifiStatusResponse{}}}, nil
	}
	if c.GetWifiList() != nil {
		return &pb.BluetoothResponse{Response: &pb.BluetoothResponse_WifiList{WifiList: &pb.WifiListResponse{Networks: []*pb.WifiNetworkInfo{{Ssid: "test-network"}}}}}, nil
	}
	if c.GetSetupEnroll() == nil {
		return nil, errors.New("unexpected command")
	}
	f.writes++
	f.lastRequest = c.GetSetupEnroll()
	if f.rejectEnrollment {
		return &pb.BluetoothResponse{Response: &pb.BluetoothResponse_Error{Error: &pb.ErrorResponse{Message: "private upstream credential error"}}}, nil
	}
	if f.loseReply {
		return nil, errors.New("link dropped")
	}
	return &pb.BluetoothResponse{Response: &pb.BluetoothResponse_SetupEnroll{SetupEnroll: &pb.SetupEnrollResponse{Pending: !f.wendyOS, PrincipalUri: "principal"}}}, nil
}
func (f *nearbyFake) Close() { f.closed++ }
func nearbyTestHooks(t *testing.T) {
	t.Helper()
	connect, prepare, check := nearbyConnectFn, nearbyPrepareFn, nearbyCheckFn
	t.Cleanup(func() { nearbyConnectFn, nearbyPrepareFn, nearbyCheckFn = connect, prepare, check })
}
func TestNearbyLostReplyRetainsAttemptAcrossReconnect(t *testing.T) {
	nearbyTestHooks(t)
	b := new(Backend)
	var peers []*nearbyFake
	nearbyConnectFn = func(context.Context, string) (nearbyConnection, error) {
		f := &nearbyFake{loseReply: true}
		peers = append(peers, f)
		return f, nil
	}
	prepared := 0
	secret := []byte("one-time fixture credential")
	credential := &commands.DesktopNearbyCredential{Request: &pb.SetupEnrollCommand{LiteConfig: secret}, DeviceID: "id", Principal: "principal", Lite: true, RequestedAt: time.Now()}
	nearbyPrepareFn = func(_ context.Context, _, _, _ string, _ *pb.SetupStatusResponse, before func()) (*commands.DesktopNearbyCredential, error) {
		prepared++
		before()
		return credential, nil
	}
	s, err := b.NearbyOpen("board")
	if err != nil {
		t.Fatal(err)
	}
	if s.WiFiConnected {
		t.Fatal("test requires no configured Wi-Fi")
	}
	s, err = b.NearbyAction(s.Session, 3, "", "", "account", "org", "board")
	if err == nil || !s.Attempted || !s.Pending || s.Connected || s.Completed {
		t.Fatalf("lost reply state: %+v %v", s, err)
	}
	if credential.Request != nil {
		t.Fatal("retained enrollment secret")
	}
	for _, v := range secret {
		if v != 0 {
			t.Fatal("retained serialized credential bytes")
		}
	}
	if len(peers[0].nonce) != 2 || peers[0].nonce[0] || !peers[0].nonce[1] {
		t.Fatal("nonce must be requested only immediately before enrollment")
	}
	s, err = b.NearbyOpen("board")
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.NearbyAction(s.Session, 3, "", "", "account", "org", "board")
	if err == nil || prepared != 1 || peers[1].writes != 0 {
		t.Fatal("reconnect allowed duplicate enrollment")
	}
	nearbyCheckFn = func(context.Context, *commands.DesktopNearbyCredential) (bool, error) { return false, nil }
	s, err = b.NearbyAction(s.Session, 4, "", "", "", "", "")
	if err != nil || s.Completed || !s.Pending {
		t.Fatal("reported success without cloud presence")
	}
	nearbyCheckFn = func(context.Context, *commands.DesktopNearbyCredential) (bool, error) { return true, nil }
	s, err = b.NearbyAction(s.Session, 4, "", "", "", "", "")
	if err != nil || !s.Completed || s.Pending {
		t.Fatal("verified cloud presence did not complete enrollment")
	}
}
func TestNearbyPreparationRetryStopsAtFirstPossibleMutation(t *testing.T) {
	nearbyTestHooks(t)
	b := new(Backend)
	f := new(nearbyFake)
	nearbyConnectFn = func(context.Context, string) (nearbyConnection, error) { return f, nil }
	attempts := 0
	nearbyPrepareFn = func(_ context.Context, _, _, _ string, _ *pb.SetupStatusResponse, before func()) (*commands.DesktopNearbyCredential, error) {
		attempts++
		if attempts == 2 {
			before()
		}
		return nil, errors.New("preparation failed")
	}
	s, _ := b.NearbyOpen("board")
	s, _ = b.NearbyAction(s.Session, 3, "", "", "account", "org", "board")
	if s.Attempted {
		t.Fatal("preflight failure prevented retry")
	}
	s, _ = b.NearbyAction(s.Session, 3, "", "", "account", "org", "board")
	if !s.Attempted {
		t.Fatal("mint uncertainty was forgotten")
	}
	_, _ = b.NearbyAction(s.Session, 3, "", "", "account", "org", "board")
	if attempts != 2 || f.writes != 0 {
		t.Fatal("retried a possibly mutating request")
	}
}
func TestNearbyStaleSessionCannotActOrCloseReplacement(t *testing.T) {
	nearbyTestHooks(t)
	b := new(Backend)
	var current *nearbyFake
	nearbyConnectFn = func(context.Context, string) (nearbyConnection, error) {
		current = new(nearbyFake)
		return current, nil
	}
	old, _ := b.NearbyOpen("board")
	next, _ := b.NearbyOpen("board")
	b.NearbyClose(old.Session)
	if current.closed != 0 {
		t.Fatal("stale cleanup closed replacement session")
	}
	if _, err := b.NearbyAction(old.Session, 0, "", "", "", "", ""); err == nil {
		t.Fatal("stale action accepted")
	}
	b.NearbyClose(next.Session)
	if current.closed != 1 {
		t.Fatal("active session did not close")
	}
}

func TestNearbyCloudRejectionAllowsExplicitCorrection(t *testing.T) {
	nearbyTestHooks(t)
	b := new(Backend)
	f := new(nearbyFake)
	nearbyConnectFn = func(context.Context, string) (nearbyConnection, error) { return f, nil }
	calls := 0
	nearbyPrepareFn = func(_ context.Context, _, _, _ string, _ *pb.SetupStatusResponse, before func()) (*commands.DesktopNearbyCredential, error) {
		calls++
		before()
		return nil, &commands.NearbyEnrollmentRejectedError{Message: "That device name is already in use. Choose another name."}
	}
	s, _ := b.NearbyOpen("board")
	s, err := b.NearbyAction(s.Session, 3, "", "", "account", "org", "taken-name")
	if err == nil || s.Attempted || s.Pending || !s.Connected {
		t.Fatalf("refusal blocked correction: %+v %v", s, err)
	}
	_, _ = b.NearbyAction(s.Session, 3, "", "", "account", "org", "new-name")
	if calls != 2 || f.writes != 0 {
		t.Fatal("explicit correction did not reach Cloud, or wrote to device")
	}
}
func TestNearbyUnknownMintReplyKeepsCheckingWithoutSendingEnrollment(t *testing.T) {
	nearbyTestHooks(t)
	b := new(Backend)
	f := new(nearbyFake)
	nearbyConnectFn = func(context.Context, string) (nearbyConnection, error) { return f, nil }
	c := &commands.DesktopNearbyCredential{DeviceID: "requested-id", Organization: "org"}
	calls := 0
	nearbyPrepareFn = func(_ context.Context, _, _, _ string, _ *pb.SetupStatusResponse, before func()) (*commands.DesktopNearbyCredential, error) {
		calls++
		before()
		return c, errors.New("Cloud reply lost")
	}
	s, _ := b.NearbyOpen("board")
	s, err := b.NearbyAction(s.Session, 3, "", "", "account", "org", "board")
	if err == nil || !s.Attempted || !s.Pending || s.DeviceID != c.DeviceID {
		t.Fatalf("lost pending identity: %+v %v", s, err)
	}
	nearbyCheckFn = func(_ context.Context, got *commands.DesktopNearbyCredential) (bool, error) {
		if got != c {
			t.Fatal("checking a different identity")
		}
		return false, nil
	}
	s, err = b.NearbyAction(s.Session, 4, "", "", "", "", "")
	if err != nil || !s.Pending || s.Completed {
		t.Fatalf("check lost pending state: %+v %v", s, err)
	}
	_, _ = b.NearbyAction(s.Session, 3, "", "", "account", "org", "board")
	if calls != 1 || f.writes != 0 {
		t.Fatal("repeated uncertain mint or sent missing credentials")
	}
}

func TestNearbyPendingEnrollmentStillAllowsWiFiDiscovery(t *testing.T) {
	nearbyTestHooks(t)
	b := new(Backend)
	f := new(nearbyFake)
	nearbyConnectFn = func(context.Context, string) (nearbyConnection, error) { return f, nil }
	s, err := b.NearbyOpen("board")
	if err != nil {
		t.Fatal(err)
	}
	b.nearby.attempts["board"] = &nearbyAttempt{attempted: true, credential: &commands.DesktopNearbyCredential{DeviceID: "pending-device"}}
	s, err = b.NearbyAction(s.Session, 1, "", "", "", "", "")
	if err != nil || !s.Pending || len(s.Networks) != 1 || s.Networks[0] != "test-network" || f.writes != 0 {
		t.Fatalf("pending enrollment blocked Wi-Fi scan or repeated enrollment: %+v %v", s, err)
	}
}

func TestNearbyDeviceFailureResumesSameCredentialOnlyAfterExplicitAction(t *testing.T) {
	nearbyTestHooks(t)
	b := new(Backend)
	f := &nearbyFake{wendyOS: true, rejectEnrollment: true}
	nearbyConnectFn = func(context.Context, string) (nearbyConnection, error) { return f, nil }
	request := &pb.SetupEnrollCommand{DeviceId: "same-device", EabKeyId: "fixture", EabHmacKey: "fixture"}
	credential := &commands.DesktopNearbyCredential{Request: request, DeviceID: "same-device", Principal: "principal", RequestedAt: time.Now()}
	mints := 0
	nearbyPrepareFn = func(_ context.Context, _, _, _ string, _ *pb.SetupStatusResponse, before func()) (*commands.DesktopNearbyCredential, error) {
		mints++
		before()
		return credential, nil
	}
	s, _ := b.NearbyOpen("board")
	s, err := b.NearbyAction(s.Session, 3, "", "", "account", "org", "device")
	if err == nil || s.Pending || !s.CanResume || s.Failure == "" || strings.Contains(s.Failure, "private") {
		t.Fatalf("failure was treated as pending: %+v %v", s, err)
	}
	nearbyCheckFn = func(context.Context, *commands.DesktopNearbyCredential) (bool, error) {
		t.Fatal("polled a known failed enrollment")
		return false, nil
	}
	_, _ = b.NearbyAction(s.Session, 4, "", "", "", "", "")
	s, _ = b.NearbyAction(s.Session, 0, "", "", "", "", "")
	if f.writes != 1 || !s.CanResume || s.Failure == "" {
		t.Fatal("background reads replayed or forgot failed enrollment")
	}
	s, _ = b.NearbyOpen("board")
	f.rejectEnrollment = false
	s, err = b.NearbyAction(s.Session, 3, "", "", "other-account", "other-org", "changed-name")
	if err != nil || !s.Completed || s.Pending || s.CanResume || s.Failure != "" || mints != 1 || f.writes != 2 || f.lastRequest != request {
		t.Fatalf("continuation did not use the original credential: %+v %v, mints=%d writes=%d", s, err, mints, f.writes)
	}
	if credential.Request != nil {
		t.Fatal("retained credential after success")
	}
}

func TestNearbyUncertainEnrollmentStopsPollingAtDeadline(t *testing.T) {
	nearbyTestHooks(t)
	b := new(Backend)
	f := new(nearbyFake)
	nearbyConnectFn = func(context.Context, string) (nearbyConnection, error) { return f, nil }
	s, _ := b.NearbyOpen("board")
	b.nearby.attempts["board"] = &nearbyAttempt{attempted: true, credential: &commands.DesktopNearbyCredential{DeviceID: "id"}, deadline: time.Now().Add(-time.Second)}
	nearbyCheckFn = func(context.Context, *commands.DesktopNearbyCredential) (bool, error) {
		t.Fatal("kept polling after deadline")
		return false, nil
	}
	s, err := b.NearbyAction(s.Session, 4, "", "", "", "", "")
	if err == nil || s.Pending || s.CanResume || s.Completed || s.Failure == "" {
		t.Fatalf("deadline left a spinner: %+v %v", s, err)
	}
	_, _ = b.NearbyAction(s.Session, 3, "", "", "account", "org", "name")
	if f.writes != 0 {
		t.Fatal("replayed ambiguous enrollment after timeout")
	}
}

func TestNearbyExpiredContinuationClearsCredential(t *testing.T) {
	c := &commands.DesktopNearbyCredential{Request: &pb.SetupEnrollCommand{EabHmacKey: "fixture"}, RequestedAt: time.Now().Add(-16 * time.Minute)}
	a := &nearbyAttempt{attempted: true, credential: c, canResume: true, failure: "offline"}
	a.expire(time.Now())
	if a.canResume || c.Request != nil || a.failure == "offline" {
		t.Fatal("expired credential remained usable")
	}
}
