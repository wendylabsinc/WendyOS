package ros2camera

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/wendylabsinc/wendy/go/internal/rtps"
)

type startLease struct {
	*fakeDiscoveryLease
	subscribed chan struct{}
}

func (l *startLease) Subscribe(ep rtps.Endpoint) error {
	if err := l.fakeDiscoveryLease.Subscribe(ep); err != nil {
		return err
	}
	select {
	case l.subscribed <- struct{}{}:
	default:
	}
	return nil
}

type brokenWriter struct{ err error }

func (w brokenWriter) WriteFrame(Frame) error { return w.err }
func (brokenWriter) Close() error             { return nil }

type startLoopback struct {
	fakeLoopback
	err     error
	missing bool
}

func (l *startLoopback) EnsureNode(ctx context.Context, id uint32, label string) error {
	if l.err != nil {
		return l.err
	}
	return l.fakeLoopback.EnsureNode(ctx, id, label)
}
func (l *startLoopback) NodePath(id uint32) (string, bool) {
	if l.missing {
		return "", false
	}
	return l.fakeLoopback.NodePath(id)
}

func TestAcquirePreservesSpecificStartFailureAndRecovers(t *testing.T) {
	good := newCDRBuilder()
	good.header()
	good.str("jpeg")
	good.bytes(testJPEG(t, 5, 4))
	bad := newCDRBuilder()
	bad.header()
	bad.str("png")
	bad.bytes([]byte("not JPEG"))
	writeErr := errors.New("configuring /dev/video128: invalid argument")
	for _, tc := range []struct {
		name             string
		payload          []byte
		writerErr, cause error
		want             string
	}{
		{"no samples", nil, nil, nil, "timed out waiting for the first frame on /camera/image: no samples received from the publisher"},
		{"undecodable", bad.b, nil, ErrUnsupportedEncoding, "timed out waiting for the first frame on /camera/image: samples received but undecodable: unsupported ROS 2 image encoding: compressed frame is not JPEG"},
		{"loopback write", good.b, writeErr, writeErr, "ROS 2 camera loopback failed: configuring /dev/video128: invalid argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			m := NewManager(ctx, zap.NewNop(), &fakeLoopback{}, filepath.Join(t.TempDir(), "registry.json"), nil, nil)
			t.Cleanup(m.Shutdown)
			m.frameWait = 100 * time.Millisecond
			m.newWriter = func(string) cameraWriter { return brokenWriter{tc.writerErr} }
			lease := &startLease{newFakeLease("lo"), make(chan struct{}, 1)}
			ep := rtps.Endpoint{GUID: rtps.GUID{EntityID: 7}, Topic: "rt/camera/image", Type: TypeCompressedImage}
			m.registerEndpoint(&participantState{participant: lease, iface: "lo"}, ep)
			acquire := func(payload []byte) error {
				result := make(chan error, 1)
				go func() {
					_, release, err := m.Acquire(ctx, IDBandStart)
					if release != nil {
						release()
					}
					result <- err
				}()
				select {
				case <-lease.subscribed:
				case <-ctx.Done():
					t.Fatal("never subscribed")
				}
				if payload != nil {
					m.handleSample(lease, rtps.Sample{Writer: ep.GUID, Payload: payload})
				}
				select {
				case err := <-result:
					return err
				case <-ctx.Done():
					t.Fatal("Acquire did not finish")
					return nil
				}
			}
			err := acquire(tc.payload)
			if err == nil || err.Error() != tc.want || tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("error=%v want=%s cause=%v", err, tc.want, tc.cause)
			}
			// A retry without samples must not report the previous attempt's
			// decode/write error, even when no writer was ever opened.
			if err := acquire(nil); err == nil || err.Error() != "timed out waiting for the first frame on /camera/image: no samples received from the publisher" {
				t.Fatalf("stale retry error: %v", err)
			}
			m.newWriter = func(string) cameraWriter { return &fakeWriter{} }
			if err := acquire(good.b); err != nil {
				t.Fatalf("recovery: %v", err)
			}
		})
	}
}

func TestAcquireReportsLoopbackCreationAndMissingNode(t *testing.T) {
	unsupported := errors.New("running build lacks compatible v4l2loopback 0.15.x support")
	for _, tc := range []struct {
		name  string
		loop  Loopback
		cause error
		want  string
	}{
		{"unsupported", &startLoopback{err: unsupported}, unsupported, "ROS 2 camera loopback failed: running build lacks compatible v4l2loopback 0.15.x support"},
		{"missing", &startLoopback{missing: true}, nil, "ROS 2 camera loopback failed: node /dev/video128 is missing"},
		{"unavailable", nil, nil, "ROS 2 camera loopback failed: loopback camera support unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs, observed := observer.New(zap.WarnLevel)
			m := NewManager(context.Background(), zap.New(logs), tc.loop, filepath.Join(t.TempDir(), "registry.json"), nil, nil)
			t.Cleanup(m.Shutdown)
			lease := newFakeLease("lo")
			m.registerEndpoint(&participantState{participant: lease}, rtps.Endpoint{GUID: rtps.GUID{EntityID: 7}, Topic: "rt/camera/image", Type: TypeImage})
			_, release, err := m.Acquire(context.Background(), IDBandStart)
			if release != nil || err == nil || err.Error() != tc.want || tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("error=%v want=%s", err, tc.want)
			}
			if len(lease.subs) != 0 {
				t.Fatal("loopback failure retained a subscription")
			}
			if tc.cause != nil && observed.FilterMessage("creating ROS 2 camera loopback failed").Len() != 1 {
				t.Fatal("loopback creation failure must warn")
			}
		})
	}
}
