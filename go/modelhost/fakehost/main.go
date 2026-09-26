// Command fakehost is a stand-in model host for testing the agent's model
// service before real hosts exist. It speaks the host side of the contract in
// internal/agent/models: it reports ready, heartbeats, and reports a person
// entering and leaving on a fixed period. It never reads the camera node it
// is given.
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

type record struct {
	Version    int            `json:"version"`
	Type       string         `json:"type"`
	Name       string         `json:"name"`
	Model      string         `json:"model,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Inputs     []sampleRef    `json:"inputs,omitempty"`
	BootID     string         `json:"boot_id"`
}

type sampleRef struct {
	SourceID string `json:"source_id"`
	SampleID uint64 `json:"sample_id"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fakehost:", err)
		os.Exit(1)
	}
}

func run() error {
	sock := os.Getenv("WENDY_DATA_SOCKET")
	if sock == "" {
		return errors.New("WENDY_DATA_SOCKET is not set")
	}
	period := 20 * time.Second
	if v := os.Getenv("FAKEHOST_PERIOD_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("FAKEHOST_PERIOD_SECONDS=%q is not a positive integer", v)
		}
		period = time.Duration(n) * time.Second
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return err
	}
	defer conn.Close()
	h := &host{conn: conn, variant: os.Getenv("WENDY_MODEL_VARIANT"), source: os.Getenv("WENDY_CAMERA_SOURCE")}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	return h.loop(stop, time.NewTicker(5*time.Second).C, time.NewTicker(period).C)
}

type host struct {
	conn    net.Conn
	variant string
	source  string
	sample  uint64
	inView  bool
}

// loop reports ready, heartbeats on every tick, and alternates a person
// entering and leaving on every period.
func (h *host) loop(stop <-chan os.Signal, heartbeat, period <-chan time.Time) error {
	if err := h.status("ready"); err != nil {
		return err
	}
	for {
		select {
		case <-stop:
			return nil
		case <-heartbeat:
			if err := h.status("ready"); err != nil {
				return err
			}
		case <-period:
			h.inView = !h.inView
			name := "model.left"
			if h.inView {
				name = "model.entered"
			}
			if err := h.detection(name); err != nil {
				return err
			}
		}
	}
}

func (h *host) status(state string) error {
	return h.send(record{Version: 1, Type: "event", Name: "model.status", BootID: "unavailable",
		Attributes: map[string]any{"state": state, "processed_fps": 10.0, "latency_p50_ms": 1.0}})
}

func (h *host) detection(name string) error {
	h.sample++
	return h.send(record{Version: 1, Type: "event", Name: name, Model: h.variant, BootID: "unavailable",
		Attributes: map[string]any{"class": "person", "confidence": 0.9, "track_id": 1,
			"box": map[string]any{"x": 0.4, "y": 0.2, "width": 0.2, "height": 0.6}},
		Inputs: []sampleRef{{SourceID: h.source, SampleID: h.sample}}})
}

// send writes one length-prefixed record and reads the agent's
// acknowledgement, framed as internal/agent/services/app_data_socket.go does.
func (h *host) send(r record) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(body)))
	if _, err := h.conn.Write(append(prefix[:], body...)); err != nil {
		return err
	}
	if _, err := io.ReadFull(h.conn, prefix[:]); err != nil {
		return err
	}
	ack := make([]byte, binary.BigEndian.Uint32(prefix[:]))
	if _, err := io.ReadFull(h.conn, ack); err != nil {
		return err
	}
	var parsed struct {
		State string `json:"state"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(ack, &parsed); err != nil {
		return err
	}
	if parsed.State == "rejected" {
		return fmt.Errorf("the agent rejected %s: %s", r.Name, parsed.Error)
	}
	return nil
}
