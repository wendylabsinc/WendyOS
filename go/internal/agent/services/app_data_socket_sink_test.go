package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
)

func TestAppDataSocketForwardsAcceptedRecordsToSink(t *testing.T) {
	capture, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	socketRoot, err := os.MkdirTemp("/tmp", "wendy-sink-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketRoot)
	oldRoot := AppDataSocketRootPath
	AppDataSocketRootPath = socketRoot
	defer func() { AppDataSocketRootPath = oldRoot }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	manager := NewAppDataSocketManager(ctx, nil, capture)
	manager.peerCred = func(net.Conn) (peerCredentials, error) { return peerCredentials{UID: 0, PID: 4242}, nil }
	manager.cgroupOfPID = func(int32) (string, error) {
		return fmt.Sprintf("0::/system.slice/%s-sh.wendy.model.m-1.scope\n", sharedenv.SystemdServiceName()), nil
	}
	type forwarded struct {
		appID string
		rec   data.ApplicationRecord
	}
	got := make(chan forwarded, 4)
	manager.SetRecordSink(func(appID string, rec data.ApplicationRecord) { got <- forwarded{appID, rec} })

	dir, err := manager.Ensure("sh.wendy.model.m-1", "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", filepath.Join(dir, DataSocketFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	send := func(rec data.ApplicationRecord) dataAck {
		body, _ := json.Marshal(rec)
		if err := writeDataFrame(conn, json.RawMessage(body)); err != nil {
			t.Fatal(err)
		}
		ackBody, err := readDataFrame(conn)
		if err != nil {
			t.Fatal(err)
		}
		var ack dataAck
		if err := json.Unmarshal(ackBody, &ack); err != nil {
			t.Fatal(err)
		}
		return ack
	}

	status := data.ApplicationRecord{Version: 1, Type: "event", Name: "model.status",
		Attributes: map[string]any{"state": "ready"}, ClientBootID: "unavailable"}
	if ack := send(status); ack.State == "rejected" {
		t.Fatalf("ack = %+v", ack)
	}
	// A record that fails validation never reaches the sink.
	if ack := send(data.ApplicationRecord{Version: 1, Type: "event", ClientBootID: "unavailable"}); ack.State != "rejected" {
		t.Fatalf("a nameless event was accepted: %+v", ack)
	}
	select {
	case f := <-got:
		if f.appID != "sh.wendy.model.m-1" || f.rec.Name != "model.status" || f.rec.Attributes["state"] != "ready" {
			t.Fatalf("forwarded %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("the accepted record never reached the sink")
	}
	select {
	case f := <-got:
		t.Fatalf("a rejected record reached the sink: %+v", f)
	default:
	}
}
