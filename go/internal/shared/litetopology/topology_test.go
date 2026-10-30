package litetopology

import (
	"context"
	"encoding/hex"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	lite "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func fixture(t *testing.T) *lite.WendyComNetTopologyPage {
	t.Helper()
	raw, err := os.ReadFile("testdata/topology_page.hex")
	if err != nil {
		t.Fatal(err)
	}
	wire, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	message := new(lite.WendyComMessage)
	if err := proto.Unmarshal(wire, message); err != nil {
		t.Fatal(err)
	}
	if message.GetResponse().GetRequestId() != 7 {
		t.Fatal("C codec fixture lost request ID")
	}
	return message.GetResponse().GetNetTopology()
}
func TestNanopbPagesNormalizeLiteReports(t *testing.T) {
	first := fixture(t)
	calls := 0
	snapshot, err := Read(context.Background(), func(_ context.Context, q *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		calls++
		page := proto.Clone(first).(*lite.WendyComNetTopologyPage)
		if q.Cursor == 1 {
			if q.Generation != 42 {
				t.Fatal(q)
			}
			page.NextCursor = 2
			page.Record.Kind = 2
			page.Record.Local = false
			page.Record.Authorized = true
			page.Record.ExpiresAtUnixMs = 1800000300000
			page.Record.NextHopDevice = page.Record.Device
		}
		if q.Cursor == 2 {
			page.NextCursor = 0
			page.Record.Kind = 3
			page.Record.Authorized = true
			page.Partial = false
			page.Record.Revision = 90
			page.Record.ExpiresAtUnixMs = 1800000300000
		}
		return page, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(snapshot.Peers) != 1 || len(snapshot.Routes) != 1 || len(snapshot.Announcements) != 1 || snapshot.Status.State != "mesh-active" || snapshot.Peers[0].Transport != "ble" || !snapshot.Routes[0].OriginAuthorized || snapshot.Announcements[0].Revision != 90 {
		t.Fatal(calls, snapshot)
	}
	if snapshot.SelfTenantId != "01020304-0506-0708-090a-0b0c0d0e0f10" || snapshot.CapturedAtUnixMs != 1800000000123 {
		t.Fatal(snapshot)
	}
}
func TestPartialAndOffReplaceOldRows(t *testing.T) {
	for _, off := range []bool{false, true} {
		page := fixture(t)
		page.NextCursor = 0
		page.Partial = !off
		if off {
			page.Record = nil
			page.RuntimeAvailable = false
			page.Generation = 0
			page.Status.State = lite.WendyComNetState_WENDY_COM_NET_OFF
			page.Status.Desired.Enabled = false
		}
		snapshot, err := Read(context.Background(), func(context.Context, *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
			return page, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if off {
			if snapshot.RuntimeAvailable || len(snapshot.Peers) != 0 || snapshot.Status.Configured.Participate || snapshot.Status.State != "disabled" {
				t.Fatal(snapshot)
			}
		} else if !snapshot.Truncated || !strings.Contains(snapshot.Status.Detail, "Partial endpoint directory") {
			t.Fatal(snapshot)
		}
	}
}
func TestChangedViewRestartsOnceWithoutMixingRows(t *testing.T) {
	calls := 0
	snapshot, err := Read(context.Background(), func(_ context.Context, q *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		calls++
		page := fixture(t)
		if calls == 2 {
			return nil, status.Error(codes.Aborted, "normal route changed")
		}
		if calls >= 3 {
			page.Generation = 43
			page.Record.LinkId = 99
			if q.Cursor != 0 {
				page.NextCursor = 0
			}
		}
		return page, nil
	})
	if err != nil || calls != 4 || len(snapshot.Peers) != 2 || snapshot.Peers[0].LinkId != 99 {
		t.Fatal(snapshot, calls, err)
	}
	calls = 0
	_, err = Read(context.Background(), func(context.Context, *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error) {
		calls++
		return nil, status.Error(codes.Aborted, "busy")
	})
	if status.Code(err) != codes.Unavailable || calls != 2 {
		t.Fatal(calls, err)
	}
}
func TestStreamNormalHandshakePagesAndCancellation(t *testing.T) {
	client, device := net.Pipe()
	done := make(chan error, 1)
	go func() {
		defer device.Close()
		wire := &stream{conn: device}
		handshake, err := wire.receive()
		if err != nil {
			done <- err
			return
		}
		if err = wire.send(handshake); err != nil {
			done <- err
			return
		}
		for cursor := uint32(0); cursor < 2; cursor++ {
			request, err := wire.receive()
			if err != nil {
				done <- err
				return
			}
			command := request.GetCommand()
			if command.GetNetTopology().Cursor != cursor {
				done <- status.Error(codes.Internal, "cursor order")
				return
			}
			page := fixture(t)
			if cursor == 1 {
				page.NextCursor = 0
				page.Record = nil
			}
			err = wire.send(&lite.WendyComMessage{Msg: &lite.WendyComMessage_Response{Response: &lite.WendyComResponse{RequestId: command.RequestId, Data: &lite.WendyComResponse_NetTopology{NetTopology: page}}}})
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	snapshot, err := ReadStream(ctx, client)
	client.Close()
	if err != nil || len(snapshot.Peers) != 1 {
		t.Fatal(snapshot, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	client, device = net.Pipe()
	ctx, cancel = context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { _, err := ReadStream(ctx, client); finished <- err }()
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("canceled read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation retained blocked stream")
	}
	device.Close()
	client.Close()
}
