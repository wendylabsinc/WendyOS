package main

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestHostSpeaksTheDataSocketContract(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	h := &host{conn: client, variant: "fakehost", source: "v4l2:/dev/video0"}
	got := make(chan record, 4)
	go func() { // the agent's side: read a frame, acknowledge it
		for {
			var prefix [4]byte
			if _, err := io.ReadFull(server, prefix[:]); err != nil {
				return
			}
			body := make([]byte, binary.BigEndian.Uint32(prefix[:]))
			if _, err := io.ReadFull(server, body); err != nil {
				return
			}
			var r record
			_ = json.Unmarshal(body, &r)
			got <- r
			ack, _ := json.Marshal(map[string]any{"version": 1, "state": "buffered"})
			binary.BigEndian.PutUint32(prefix[:], uint32(len(ack)))
			_, _ = server.Write(append(prefix[:], ack...))
		}
	}()
	stop := make(chan os.Signal)
	heartbeat := make(chan time.Time)
	period := make(chan time.Time)
	done := make(chan error, 1)
	go func() { done <- h.loop(stop, heartbeat, period) }()

	if r := <-got; r.Name != "model.status" || r.Attributes["state"] != "ready" {
		t.Fatalf("first record = %+v", r)
	}
	period <- time.Now()
	if r := <-got; r.Name != "model.entered" || r.Attributes["class"] != "person" || r.Inputs[0].SampleID != 1 || r.Inputs[0].SourceID != "v4l2:/dev/video0" {
		t.Fatalf("second record = %+v", r)
	}
	heartbeat <- time.Now()
	if r := <-got; r.Name != "model.status" {
		t.Fatalf("heartbeat record = %+v", r)
	}
	period <- time.Now()
	if r := <-got; r.Name != "model.left" {
		t.Fatalf("fourth record = %+v", r)
	}
	stop <- syscall.SIGTERM
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
