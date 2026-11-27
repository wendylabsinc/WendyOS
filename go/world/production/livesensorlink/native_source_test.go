package livesensorlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensorlink"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	captureapp "github.com/wendylabsinc/wendy/go/world/production/livecapture"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
)

func nativeSourceFixture(t *testing.T, cfg NativeSourceConfig, seq uint64) (captureapp.Capture, []byte) {
	t.Helper()
	snap := testSnapshot(t, seq, cfg.Commission.BootID)
	request, media, err := sensorrecord.FromSnapshot(snap, "camera-b", time.Unix(0, cfg.Commission.AnchorUnixNS), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c, err := sensorrecord.Decode(request.Payload)
	if err != nil {
		t.Fatal(err)
	}
	value := captureapp.Capture{Capture: c, Media: media}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return value, raw
}

func nativeConnect(t *testing.T, address string, channels []uint32) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	envelope, err := sensorlink.ReadMessage(conn)
	if err != nil || envelope.GetManifest() == nil {
		t.Fatalf("manifest: %v", err)
	}
	manifest := envelope.GetManifest()
	if len(manifest.Sensors) != 1 || manifest.Sensors[0].GetVideo().Width != 16 || manifest.Sensors[0].GetVideo().Height != 12 {
		t.Fatal("manifest dimensions")
	}
	if err = sensorlink.WriteMessage(conn, &pb.Envelope{Msg: &pb.Envelope_Subscribe{Subscribe: &pb.Subscribe{ChannelId: channels}}}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn
}

func nativeReadFrame(t *testing.T, conn net.Conn) *sensorlink.Frame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	assembler, _ := sensorlink.NewAssembler([]uint32{4}, 0)
	for {
		env, err := sensorlink.ReadMessage(conn)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := assembler.Add(env.GetData())
		if err != nil {
			t.Fatal(err)
		}
		if frame != nil {
			_ = conn.SetReadDeadline(time.Time{})
			return frame
		}
	}
}

func TestNativeSourceManifestPeekSteppedFramesAndOriginalHold(t *testing.T) {
	cfg := NativeSourceConfig{SourceConfig: testSourceConfig(t), Width: 16, Height: 12}
	cfg.PollIntervalMS = 50
	var raw atomic.Value
	raw.Store([]byte(nil))
	var polls atomic.Int32
	scene := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/captures/camera-b" || r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("capture request identity")
			w.WriteHeader(403)
			return
		}
		polls.Add(1)
		b := raw.Load().([]byte)
		if b == nil {
			w.WriteHeader(204)
			return
		}
		_, _ = w.Write(b)
	}))
	defer scene.Close()
	cfg.SceneURL = scene.URL
	metaListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MetadataListen = metaListener.Addr().String()
	metaListener.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	cfg.Ready = func() error { close(ready); return nil }
	type artifact struct {
		meta Metadata
		jpeg []byte
	}
	artifacts := make(chan artifact, 2)
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		r, e := RunNativeSourceWithListener(ctx, cfg, listener, func(m Metadata, b []byte) error { artifacts <- artifact{m, b}; return nil })
		done <- outcome{r, e}
	}()
	<-ready
	peek := nativeConnect(t, listener.Addr().String(), nil)
	one := make([]byte, 1)
	_ = peek.SetReadDeadline(time.Now().Add(time.Second))
	if _, e := peek.Read(one); e != io.EOF {
		t.Fatalf("peek must close: %v", e)
	}
	peek.Close()
	if polls.Load() != 0 {
		t.Fatal("manifest peek acquired a sample")
	}
	stream := nativeConnect(t, listener.Addr().String(), []uint32{4})
	defer stream.Close()
	first, firstRaw := nativeSourceFixture(t, cfg, 17)
	raw.Store(firstRaw)
	frame1 := nativeReadFrame(t, stream)
	saved1 := <-artifacts
	want1, _ := sensorrecord.Encode(first.Capture)
	if frame1.Seq != 1 || frame1.TsUs != uint64(first.Capture.CapturedUnixNS/1000) || !bytes.Equal(frame1.Payload, saved1.jpeg) || !bytes.Equal(saved1.meta.CaptureJSON, want1) {
		t.Fatal("first original changed")
	}
	// A held capture and then a disabled source must not mint additional frames.
	time.Sleep(120 * time.Millisecond)
	raw.Store([]byte(nil))
	time.Sleep(80 * time.Millisecond)
	_ = stream.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, e := sensorlink.ReadMessage(stream); e == nil {
		t.Fatal("duplicate or disabled capture sent")
	}
	_ = stream.SetReadDeadline(time.Time{})
	second, secondRaw := nativeSourceFixture(t, cfg, 19)
	raw.Store(secondRaw)
	frame2 := nativeReadFrame(t, stream)
	saved2 := <-artifacts
	want2, _ := sensorrecord.Encode(second.Capture)
	if frame2.Seq != 2 || frame2.TsUs != uint64(second.Capture.CapturedUnixNS/1000) || !bytes.Equal(frame2.Payload, saved2.jpeg) || !bytes.Equal(saved2.meta.CaptureJSON, want2) {
		t.Fatal("restored original changed")
	}
	req, _ := http.NewRequest("GET", "http://"+cfg.MetadataListen+metadataPath(saved2.meta), nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	var meta Metadata
	if response.StatusCode != 200 || json.Unmarshal(body, &meta) != nil || !bytes.Equal(meta.CaptureJSON, want2) {
		t.Fatal("exact metadata unavailable")
	}
	_ = stream.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	if _, e := stream.Read(one); e == io.EOF {
		t.Fatal("final native session closed before original expiry")
	}
	result := <-done
	if result.err != nil || result.result.Status != "complete" || result.result.Mode != "native-source" || result.result.FramesSent != 2 {
		t.Fatalf("result: %+v %v", result.result, result.err)
	}
	if time.Now().Before(time.Unix(0, second.Capture.CapturedUnixNS).Add(MaxCaptureAge)) {
		t.Fatal("final hold ended early")
	}
	if _, e := net.DialTimeout("tcp", listener.Addr().String(), time.Second); e == nil {
		t.Fatal("listener leaked")
	}
}

func TestNativeSourceRejectsAlteredOriginalAndManifestMismatch(t *testing.T) {
	cfg := NativeSourceConfig{SourceConfig: testSourceConfig(t), Width: 16, Height: 12}
	base, _ := nativeSourceFixture(t, cfg, 17)
	for _, name := range []string{"submicrosecond", "stale", "wrong-dimensions", "already-bound"} {
		t.Run(name, func(t *testing.T) {
			c := base
			testCfg := cfg
			switch name {
			case "submicrosecond":
				c.Capture.CapturedUnixNS++
			case "stale":
				c.Capture.CapturedUnixNS = time.Now().Add(-6*time.Second).UnixMicro() * 1000
			case "wrong-dimensions":
				testCfg.Width++
			case "already-bound":
				c.Capture.SensorLink = &sensorrecord.WireBinding{}
			}
			if _, err := prepareNativeCapture(c, testCfg, 1); err == nil {
				t.Fatal("altered original accepted")
			}
		})
	}
}

func TestNativeSourceCanceledBeforeCaptureClosesPeekListener(t *testing.T) {
	cfg := NativeSourceConfig{SourceConfig: testSourceConfig(t), Width: 16, Height: 12}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := RunNativeSourceWithListener(ctx, cfg, l, nil)
	if !errors.Is(err, context.Canceled) || r.Status != "failed" {
		t.Fatalf("cancel: %v %+v", err, r)
	}
}

func TestNativeSourceConflictingHeldCaptureStopsWithoutNewWireVersion(t *testing.T) {
	cfg := NativeSourceConfig{SourceConfig: testSourceConfig(t), Width: 16, Height: 12}
	cfg.PollIntervalMS = 50
	capture, original := nativeSourceFixture(t, cfg, 17)
	var body atomic.Value
	body.Store(original)
	scene := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body.Load().([]byte)) }))
	defer scene.Close()
	cfg.SceneURL = scene.URL
	server, client := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	result := Result{}
	go func() {
		_, err := nativeSourceConn(ctx, server, cfg, boundedHTTP(time.Second), testToken, newCache(testToken), nil, &result)
		done <- err
	}()
	if env, err := sensorlink.ReadMessage(client); err != nil || env.GetManifest() == nil {
		t.Fatalf("manifest: %v", err)
	}
	if err := sensorlink.WriteMessage(client, &pb.Envelope{Msg: &pb.Envelope_Subscribe{Subscribe: &pb.Subscribe{ChannelId: []uint32{4}}}}); err != nil {
		t.Fatal(err)
	}
	frame := nativeReadFrame(t, client)
	if frame.Seq != 1 {
		t.Fatal("initial wire version")
	}
	capture.Capture.CapturedUnixNS += 1000
	changed, _ := json.Marshal(capture)
	body.Store(changed)
	if err := <-done; !errors.Is(err, ErrCapture) {
		t.Fatalf("conflicting same sample accepted: %v", err)
	}
	if result.FramesSent != 1 {
		t.Fatal("conflict minted a wire version")
	}
}
