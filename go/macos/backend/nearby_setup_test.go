package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type nearbyFake struct {
	closed, writes int
	nonce          []bool
	loseReply      bool
}

func (f *nearbyFake) Status(_ context.Context, nonce bool) (*pb.SetupStatusResponse, error) {
	f.nonce = append(f.nonce, nonce)
	return &pb.SetupStatusResponse{DeviceClass: "C", LiteSupported: true}, nil
}
func (f *nearbyFake) Call(_ context.Context, c *pb.BluetoothCommand) (*pb.BluetoothResponse, error) {
	if c.GetSetupEnroll() == nil {
		return nil, errors.New("unexpected command")
	}
	f.writes++
	if f.loseReply {
		return nil, errors.New("link dropped")
	}
	return &pb.BluetoothResponse{Response: &pb.BluetoothResponse_SetupEnroll{SetupEnroll: &pb.SetupEnrollResponse{Pending: true, PrincipalUri: "principal"}}}, nil
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
