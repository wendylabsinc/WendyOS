package services

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

type recordingSocket struct {
	listener    *net.UnixListener
	service     string
	name        string
	config      appconfig.RecordingStream
	connections map[*net.UnixConn]struct{} // protected by manager.mu
	closed      bool
}

func (m *AppDataSocketManager) EnsureStreams(appID, service string, streams map[string]appconfig.RecordingStream) (string, error) {
	if err := appconfig.ValidateRecordingStreams(streams); err != nil {
		return "", err
	}
	dir, err := m.Ensure(appID, service)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sockets[appDataKey(appID)]
	if s == nil {
		return "", errors.New("app data socket released during configuration")
	}
	if s.streams == nil {
		s.streams = map[string]*recordingSocket{}
	}
	for _, old := range s.streams {
		if old.service == service {
			cfg, ok := streams[old.name]
			if !ok || !reflect.DeepEqual(cfg, old.config) {
				return "", errors.New("release service before changing its recording streams")
			}
		}
	}
	namespace := filepath.Join(dir, appconfig.RecordingServiceDirectory(service))
	if err = os.MkdirAll(namespace, 0o750); err != nil {
		return "", err
	}
	if err = os.Chmod(namespace, 0o750); err != nil {
		return "", err
	}
	if os.Geteuid() == 0 {
		if err = os.Chown(namespace, 0, dataSocketGroupGID); err != nil {
			return "", err
		}
	}
	created := map[string]*recordingSocket{}
	cleanup := func() {
		for _, r := range created {
			r.listener.Close()
			os.Remove(r.listener.Addr().String())
		}
	}
	for name, cfg := range streams {
		key := service + "/" + name
		if s.streams[key] != nil {
			continue
		}
		path := filepath.Join(namespace, name+".sock")
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			cleanup()
			return "", err
		}
		l, e := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
		if e != nil {
			cleanup()
			return "", fmt.Errorf("recording streams require Unix SOCK_SEQPACKET: %w", e)
		}
		l.SetUnlinkOnClose(false)
		r := &recordingSocket{listener: l, service: service, name: name, config: cfg, connections: map[*net.UnixConn]struct{}{}}
		created[key] = r
		if e = os.Chmod(path, 0o660); e == nil && os.Geteuid() == 0 {
			e = os.Chown(path, 0, dataSocketGroupGID)
		}
		if e != nil {
			cleanup()
			return "", e
		}
	}
	for key, r := range created {
		s.streams[key] = r
		go m.serveRecording(s, r)
	}
	return dir, nil
}
func (m *AppDataSocketManager) closeRecordingSockets(s *appDataSocket, service *string) {
	// Caller holds m.mu. Closing active peers also revokes an already-open grant.
	for key, r := range s.streams {
		if service != nil && r.service != *service {
			continue
		}
		r.closed = true
		r.listener.Close()
		os.Remove(r.listener.Addr().String())
		for c := range r.connections {
			c.Close()
		}
		delete(s.streams, key)
	}
}
func (m *AppDataSocketManager) serveRecording(s *appDataSocket, r *recordingSocket) {
	for {
		c, err := r.listener.AcceptUnix()
		if err != nil {
			return
		}
		if !m.admit(s) {
			c.Close()
			continue
		}
		m.mu.Lock()
		if r.closed {
			m.mu.Unlock()
			c.Close()
			m.finish(s)
			continue
		}
		r.connections[c] = struct{}{}
		m.mu.Unlock()
		go func() {
			defer func() { c.Close(); m.mu.Lock(); delete(r.connections, c); m.mu.Unlock(); m.finish(s) }()
			m.serveRecordingConn(s, r, c)
		}()
	}
}
func (m *AppDataSocketManager) serveRecordingConn(s *appDataSocket, r *recordingSocket, c *net.UnixConn) {
	if m.verifyPeer(s.appID, c) != nil {
		return
	}
	// Allocate once per bounded connection. ReadMsgUnix exposes MSG_TRUNC so an
	// oversized packet cannot be silently recorded as a successful shorter one.
	buf := make([]byte, data.MaxRecordingPacket)
	for {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Minute))
		n, _, flags, _, err := c.ReadMsgUnix(buf, nil)
		if err != nil || n == 0 {
			return
		}
		if !s.limiter.allow(time.Now()) {
			return
		}
		ack := &recordingpb.Ack{State: recordingpb.Ack_REJECTED}
		var record *recordingpb.Record
		if flags&unix.MSG_TRUNC != 0 {
			ack.Error = "record exceeds 1 MiB"
		} else if r.config.Mode == "durable" {
			record = new(recordingpb.Record)
			if err = proto.Unmarshal(buf[:n], record); err != nil {
				ack.Error = "invalid recording protobuf"
			} else {
				if len(record.Id) <= 128 {
					ack.Id = record.Id
				}
			}
		} else {
			record = &recordingpb.Record{Payload: buf[:n]}
		}
		if ack.Error == "" {
			duplicate, e := m.capture.RecordStream(s.appID, r.service, r.name, r.config, record)
			if e != nil {
				ack.Error = e.Error()
			} else if duplicate {
				ack.State = recordingpb.Ack_DUPLICATE
			} else {
				ack.State = recordingpb.Ack_COMMITTED
			}
		}
		if r.config.Mode == "lightweight" {
			// No acknowledgement protocol in the raw path. A failure terminates the
			// connection; a successful send alone never proves agent acceptance.
			if ack.Error != "" {
				return
			}
			continue
		}
		b, err := proto.Marshal(ack)
		if err != nil {
			return
		}
		_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err = c.Write(b); err != nil {
			return
		}
	}
}
