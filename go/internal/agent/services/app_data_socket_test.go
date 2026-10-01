package services

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
)

func TestDataProtocolLengthLimit(t *testing.T) {
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], dataProtocolMaxRecord+1)
	if _, err := readDataFrame(bytes.NewReader(h[:])); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestAppDataSocketIsPrivateAndRecordsIdentity(t *testing.T) {
	capture, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	socketRoot, err := os.MkdirTemp("/tmp", "wendy-data-test-")
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
	// The data socket is hardened: once SO_PEERCRED yields a peer pid,
	// verifyPeer fails closed unless that pid's cgroup resolves to the socket's
	// own app. The go-test process is not in a wendy app scope, so on Linux (the
	// target platform, where SO_PEERCRED succeeds) an uninjected dial is
	// correctly refused, while on macOS the no-peer fail-open branch hides that.
	// Inject the same seams the D6 hardening tests use so the happy path is
	// exercised deterministically on both platforms.
	manager.peerCred = func(net.Conn) (peerCredentials, error) {
		return peerCredentials{UID: 0, PID: 4242}, nil
	}
	manager.cgroupOfPID = func(int32) (string, error) {
		return fmt.Sprintf("0::/system.slice/%s-com.example.a.scope\n", sharedenv.SystemdServiceName()), nil
	}
	dirA, err := manager.Ensure("com.example.a", "")
	if err != nil {
		t.Fatal(err)
	}
	dirB, err := manager.Ensure("com.example.b", "")
	if err != nil {
		t.Fatal(err)
	}
	if dirA == dirB {
		t.Fatal("cross-app sockets share a directory")
	}
	conn, err := net.Dial("unix", filepath.Join(dirA, DataSocketFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	record := data.ApplicationRecord{Version: 1, Type: "event", Name: "ready", ClientBootID: "unavailable"}
	body, _ := json.Marshal(record)
	if err = writeDataFrame(conn, json.RawMessage(body)); err != nil {
		t.Fatal(err)
	}
	ackBody, err := readDataFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	var ack dataAck
	if err = json.Unmarshal(ackBody, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.State != "buffered" {
		t.Fatalf("ack=%+v", ack)
	}
	started, err := capture.Start(data.StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = capture.Stop(data.AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	manifest, failures, err := capture.Inspect(started.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) > 0 {
		t.Fatal(failures)
	}
	if len(manifest.Files) == 0 {
		t.Fatal("no sealed files")
	}
}

func TestValidateApplicationRecord(t *testing.T) {
	record := data.ApplicationRecord{Version: 1, Type: "event", Name: "started"}
	if err := validateApplicationRecord(record); err != nil {
		t.Fatal(err)
	}
	record.Version = 2
	if err := validateApplicationRecord(record); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("got %v", err)
	}
}

// TestValidateApplicationRecordAllowsInputsOnEvents covers the design §6.1
// contract: model hosts report each detection as an "event" record bound to
// the frame that triggered it, so "event" must accept the same `inputs` list
// "prediction" does, still checked by data.ValidateSampleRefs.
func TestValidateApplicationRecordAllowsInputsOnEvents(t *testing.T) {
	validInputs := []data.SampleRef{{SourceID: "v4l2:/dev/video0", SampleID: 1}}
	invalidInputs := []data.SampleRef{{SampleID: 1}} // data.ValidateSampleRefs: missing source_id

	event := data.ApplicationRecord{Version: 1, Type: "event", Name: "model.entered", Inputs: validInputs}
	if err := validateApplicationRecord(event); err != nil {
		t.Fatalf("event with valid inputs was rejected: %v", err)
	}

	event.Inputs = invalidInputs
	if err := validateApplicationRecord(event); err == nil {
		t.Fatal("event with an invalid input reference was accepted")
	}

	event.Inputs = nil
	if err := validateApplicationRecord(event); err != nil {
		t.Fatalf("event without inputs was rejected: %v", err)
	}

	prediction := data.ApplicationRecord{Version: 1, Type: "prediction", Model: "test", Inputs: validInputs}
	if err := validateApplicationRecord(prediction); err != nil {
		t.Fatalf("prediction with valid inputs was rejected: %v", err)
	}
}
