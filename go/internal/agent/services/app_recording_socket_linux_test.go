//go:build linux

package services

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"google.golang.org/protobuf/proto"
)

func recordingSocketTestManager(t *testing.T) (*AppDataSocketManager, *data.Manager) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "wr-")
	if err != nil {
		t.Fatal(err)
	}
	old := AppDataSocketRootPath
	AppDataSocketRootPath = root
	capture, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := NewAppDataSocketManager(ctx, nil, capture)
	m.cgroupOfPID = func(pid int32) (string, error) {
		if pid != int32(os.Getpid()) {
			return "", fmt.Errorf("unexpected peer pid %d", pid)
		}
		return fmt.Sprintf("0::/system.slice/%s-test.app.scope", sharedenv.SystemdServiceName()), nil
	}
	t.Cleanup(func() { cancel(); m.stopAll(); AppDataSocketRootPath = old; os.RemoveAll(root) })
	return m, capture
}
func dialRecording(t *testing.T, dir, service, name string) *net.UnixConn {
	t.Helper()
	c, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: filepath.Join(dir, appconfig.RecordingServiceDirectory(service), name+".sock"), Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { c.Close() })
	return c
}
func receiveRecordingAck(t *testing.T, c *net.UnixConn) *recordingpb.Ack {
	t.Helper()
	b := make([]byte, 4096)
	n, err := c.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	ack := new(recordingpb.Ack)
	if err = proto.Unmarshal(b[:n], ack); err != nil {
		t.Fatal(err)
	}
	return ack
}
func TestRecordingPacketDurabilityRetryAndRevocation(t *testing.T) {
	m, capture := recordingSocketTestManager(t)
	streams := map[string]appconfig.RecordingStream{"samples": {Mode: "durable", MediaType: "application/octet-stream"}}
	dir, err := m.EnsureStreams("test.app", "worker", streams)
	if err != nil {
		t.Fatal(err)
	}
	c := dialRecording(t, dir, "worker", "samples")
	record := &recordingpb.Record{Id: "retry", Payload: []byte{0, 255, 128, '\n'}}
	b, _ := proto.Marshal(record)
	for _, want := range []recordingpb.Ack_State{recordingpb.Ack_COMMITTED, recordingpb.Ack_DUPLICATE} {
		if _, err = c.Write(b); err != nil {
			t.Fatal(err)
		}
		ack := receiveRecordingAck(t, c)
		if ack.State != want || ack.Id != "retry" {
			t.Fatalf("ack: %v", ack)
		}
	}
	var count int
	err = capture.ExportRecording("test.app", "worker", "samples", func(r *recordingpb.StoredRecord) error {
		count++
		if !bytes.Equal(r.Record.Payload, record.Payload) {
			t.Fatal("payload changed")
		}
		return nil
	})
	if err != nil || count != 1 {
		t.Fatalf("export: %v %d", err, count)
	}
	c.Write([]byte{255})
	if ack := receiveRecordingAck(t, c); ack.State != recordingpb.Ack_REJECTED {
		t.Fatal("invalid protobuf accepted")
	}
	// Releasing one service must close existing connections, not just unlink
	// its listener while the old grant remains usable.
	m.Release("test.app", "worker")
	c.SetReadDeadline(time.Now().Add(time.Second))
	c.Write(b)
	if _, err = c.Read(make([]byte, 4096)); err == nil {
		t.Fatal("released socket still accepts records")
	}
}
func TestRecordingPacketRawBoundariesAndMedia(t *testing.T) {
	m, capture := recordingSocketTestManager(t)
	dir, err := m.EnsureStreams("test.app", "", map[string]appconfig.RecordingStream{"temperature": {Mode: "lightweight", MediaType: "text/csv", TimeSeries: &appconfig.RecordingTimeSeries{Clock: "CLOCK_BOOTTIME", TimestampField: "time_ns", Channels: []appconfig.RecordingChannel{{Name: "value", Type: "float64", Unit: "Cel"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	episode, err := capture.Start(data.StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	c := dialRecording(t, dir, "", "temperature")
	for _, b := range [][]byte{[]byte("100,21.5\n200,21.6\n"), {0, 255, 128}} {
		if _, err = c.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status := capture.Status()
		if status != nil && status.Sources[0].Count == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mf, err := capture.Stop(data.AdHocEpisodeKey)
	if err != nil {
		t.Fatal(err)
	}
	if mf.Sources[0].Count != 2 {
		t.Fatalf("packet boundaries lost: %v", mf.Sources)
	}
	// Retrieve the sealed episode through the ordinary episode API.
	sealed, failures, err := capture.Inspect(episode.ID, true)
	if err != nil || len(failures) > 0 || sealed.ModelIO.BinaryOutcomeLog != data.RecordingLogFile {
		t.Fatalf("episode: %v %v", err, failures)
	}
}
func TestRecordingPacketRejectsWrongPeer(t *testing.T) {
	m, _ := recordingSocketTestManager(t)
	dir, err := m.EnsureStreams("different.app", "", map[string]appconfig.RecordingStream{"samples": {Mode: "durable", MediaType: "text/plain"}})
	if err != nil {
		t.Fatal(err)
	}
	c := dialRecording(t, dir, "", "samples")
	b, _ := proto.Marshal(&recordingpb.Record{Id: "one", Payload: []byte("test")})
	c.Write(b)
	if _, err = c.Read(make([]byte, 4096)); err == nil {
		t.Fatal("wrong app admitted")
	}
}

// Exercise real Linux packet boundaries and acknowledgements while a small
// journal repeatedly fills and drains. No target-device throughput is asserted.
func TestRecordingPacketContinuousVibration(t *testing.T) {
	m, capture := recordingSocketTestManager(t)
	cfg := appconfig.RecordingStream{Mode: "durable", MediaType: "application/octet-stream", Storage: &appconfig.RecordingStorage{MaxBytes: 1 << 20, RetentionSeconds: proto.Int64(0)}}
	dir, err := m.EnsureStreams("test.app", "", map[string]appconfig.RecordingStream{"vibration": cfg})
	if err != nil {
		t.Fatal(err)
	}
	c := dialRecording(t, dir, "", "vibration")
	c.SetDeadline(time.Now().Add(20 * time.Second))
	payload := make([]byte, 2560*3*4)
	for i := range payload {
		payload[i] = byte(i)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for i := 0; i < 128; i++ {
		<-ticker.C // Stay below the existing 200-packet/sec per-app limit.
		id := fmt.Sprint(i)
		b, err := proto.Marshal(&recordingpb.Record{Id: id, Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.Write(b); err != nil {
			t.Fatal(err)
		}
		ack := receiveRecordingAck(t, c)
		if ack.State != recordingpb.Ack_COMMITTED || ack.Id != id {
			t.Fatal("batch not committed", ack)
		}
		if (i+1)%16 == 0 {
			count := 0
			token, err := capture.ExportRecordingCheckpoint("test.app", "", "vibration", true, func(r *recordingpb.StoredRecord) error {
				if r.Record.Id != fmt.Sprint(i-15+count) || !bytes.Equal(r.Record.Payload, payload) {
					return fmt.Errorf("lost, reordered or changed vibration batch")
				}
				count++
				return nil
			})
			if err != nil || count != 16 {
				t.Fatal("export", count, err)
			}
			if err = capture.AcknowledgeRecordingExport("test.app", "", "vibration", token); err != nil {
				t.Fatal(err)
			}
		}
	}
}
