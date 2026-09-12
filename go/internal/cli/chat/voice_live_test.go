package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/net/websocket"
)

// Tests never open a real microphone, connect to OpenAI, or read credentials.
type testLiveWire struct {
	in        chan []byte
	out       chan map[string]any
	done      chan struct{}
	once      sync.Once
	autoAck   bool
	autoClose bool
}

func newTestLiveWire() *testLiveWire {
	return &testLiveWire{in: make(chan []byte, 64), out: make(chan map[string]any, 64), done: make(chan struct{}), autoAck: true, autoClose: true}
}

func (w *testLiveWire) push(v any) {
	data, _ := json.Marshal(v)
	w.in <- data
}

func (w *testLiveWire) Send(ctx context.Context, v any) error {
	data, _ := json.Marshal(v)
	var event map[string]any
	_ = json.Unmarshal(data, &event)
	select {
	case w.out <- event:
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return io.ErrClosedPipe
	}
	kind, _ := event["type"].(string)
	if w.autoAck && (kind == "session.thinking.append" || kind == "session.commentary.append" || kind == "session.instructions.append") {
		w.push(map[string]any{"type": kind + "ed", "client_event_id": event["event_id"]})
	}
	if w.autoClose && kind == "session.close" {
		w.push(map[string]any{"type": "session.closed"})
	}
	return nil
}

func (w *testLiveWire) Receive(v any) error {
	select {
	case data := <-w.in:
		return json.Unmarshal(data, v)
	case <-w.done:
		return io.EOF
	}
}

func (w *testLiveWire) Close() error {
	w.once.Do(func() { close(w.done) })
	return nil
}

type testLiveAudio struct {
	input   chan []byte
	writes  chan []byte
	blocked chan struct{}
	flush   chan struct{}
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	flushes int
}

func newTestLiveAudio() *testLiveAudio {
	return &testLiveAudio{input: make(chan []byte, 16), writes: make(chan []byte, 16), flush: make(chan struct{}, 16), done: make(chan struct{})}
}

func (a *testLiveAudio) Read(p []byte) (int, error) {
	select {
	case data := <-a.input:
		return copy(p, data), nil
	case <-a.done:
		return 0, io.EOF
	}
}

func (a *testLiveAudio) Write(p []byte) (int, error) {
	a.writes <- append([]byte(nil), p...)
	if a.blocked != nil {
		select {
		case <-a.blocked:
		case <-a.flush:
			return 0, ErrVoicePlaybackInterrupted
		case <-a.done:
			return 0, io.ErrClosedPipe
		}
	}
	return len(p), nil
}

func (a *testLiveAudio) Flush() error {
	a.mu.Lock()
	a.flushes++
	a.mu.Unlock()
	select {
	case a.flush <- struct{}{}:
	default:
	}
	return nil
}

func (a *testLiveAudio) Close() error {
	a.once.Do(func() { close(a.done) })
	return nil
}

func startTestLive(t *testing.T, wire *testLiveWire, audio *testLiveAudio) *liveSession {
	t.Helper()
	wire.push(map[string]any{"type": "session.started"})
	ctx, cancel := context.WithCancel(context.Background())
	session, err := startVoice(ctx, "test-secret", func(context.Context, string) (liveWire, error) { return wire, nil }, func() (VoiceAudio, error) { return audio, nil })
	if err != nil {
		t.Fatal(err)
	}
	s := session.(*liveSession)
	t.Cleanup(func() { cancel(); _ = s.Close() })
	return s
}

func awaitLiveSent(t *testing.T, wire *testLiveWire, kind string) map[string]any {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case event := <-wire.out:
			if event["type"] == kind {
				return event
			}
		case <-timeout:
			t.Fatalf("no outbound %s", kind)
			return nil
		}
	}
}

func awaitLiveEvent(t *testing.T, session VoiceSession, kind string) VoiceEvent {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case event, ok := <-session.Events():
			if !ok {
				t.Fatalf("events closed before %s", kind)
			}
			if event.Type == kind {
				return event
			}
		case <-timeout:
			t.Fatalf("no voice event %s", kind)
			return VoiceEvent{}
		}
	}
}

func TestLiveStartupAndQuietContext(t *testing.T) {
	wire, audio := newTestLiveWire(), newTestLiveAudio()
	s := startTestLive(t, wire, audio)
	start := awaitLiveSent(t, wire, "session.start")
	spec := start["session"].(map[string]any)
	if spec["model"] != "gpt-live-1" || spec["store"] != false || spec["delegation"].(map[string]any)["type"] != "client" {
		t.Fatalf("unexpected session configuration: %#v", spec)
	}
	format := spec["audio"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "audio/pcm" || format["rate"] != float64(24000) {
		t.Fatalf("unexpected audio format: %#v", format)
	}
	awaitLiveEvent(t, s, "ready")
	if err := s.SendContext(context.Background(), "The workspace is /test/project."); err != nil {
		t.Fatal(err)
	}
	contextEvent := awaitLiveSent(t, wire, "session.thinking.append")
	if id, ok := contextEvent["delegation_id"]; !ok || id != nil {
		t.Fatalf("general context must include explicit null delegation_id: %#v", contextEvent)
	}
	select {
	case extra := <-wire.out:
		t.Fatalf("startup or context produced unwanted event: %#v", extra)
	default:
	}
}

func TestLiveAudioPreservesSampleBoundaries(t *testing.T) {
	wire, audio := newTestLiveWire(), newTestLiveAudio()
	_ = startTestLive(t, wire, audio)
	audio.input <- []byte{1, 2, 3}
	audio.input <- []byte{4, 5, 6}
	var result []byte
	for range 2 {
		event := awaitLiveSent(t, wire, "session.input_audio.append")
		data, err := base64.StdEncoding.DecodeString(event["audio"].(string))
		if err != nil || len(data)%2 != 0 {
			t.Fatalf("invalid PCM frame: %v %v", data, err)
		}
		result = append(result, data...)
	}
	if string(result) != string([]byte{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("lost microphone bytes: %v", result)
	}
}

func TestLiveDelegationContinuesWhileSpeakerBlocked(t *testing.T) {
	wire, audio := newTestLiveWire(), newTestLiveAudio()
	audio.blocked = make(chan struct{})
	s := startTestLive(t, wire, audio)
	wire.push(map[string]any{"type": "session.output_audio.delta", "delta": base64.StdEncoding.EncodeToString([]byte{1, 2})})
	select {
	case <-audio.writes:
	case <-time.After(time.Second):
		t.Fatal("playback did not start")
	}
	wire.push(map[string]any{"type": "session.input_transcript.delta", "event_id": "speech1", "delta": "Deploy the project.", "start_ms": 100, "end_ms": 500})
	wire.push(map[string]any{"type": "session.delegation.created", "offset_ms": 600, "delegation": map[string]any{"id": "opaque-id", "type": "delegation", "target": "client"}})
	event := awaitLiveEvent(t, s, "delegation")
	if event.DelegationID != "opaque-id" || !strings.Contains(event.Text, "Deploy the project.") {
		t.Fatalf("lost delegation while speaker blocked: %#v", event)
	}
	if err := s.Reply(context.Background(), "opaque-id", "Deployment completed."); err != nil {
		t.Fatal(err)
	}
	reply := awaitLiveSent(t, wire, "session.commentary.append")
	if reply["delegation_id"] != "opaque-id" {
		t.Fatalf("wrong result delegation: %#v", reply)
	}
	if err := s.Reply(context.Background(), "", "Review the terminal and press y or n."); err != nil {
		t.Fatal(err)
	}
	notice := awaitLiveSent(t, wire, "session.commentary.append")
	if notice["delegation_id"] != nil {
		t.Fatalf("approval notice consumed delegation: %#v", notice)
	}
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	stop := awaitLiveSent(t, wire, "session.instructions.append")
	if stop["delegation_id"] != nil || !strings.Contains(stop["content"].(string), "Stop speaking") {
		t.Fatalf("wrong interruption command: %#v", stop)
	}
	if s.ctx.Err() != nil {
		t.Fatal("expected playback interruption killed session")
	}
}

func TestLiveFailureClosesAudioAndRedactsCredential(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(*testLiveWire)
	}{
		{"api", func(w *testLiveWire) {
			w.push(map[string]any{"type": "error", "error": map[string]any{"message": "rejected test-secret"}})
		}},
		{"audio", func(w *testLiveWire) { w.push(map[string]any{"type": "session.output_audio.delta", "delta": "%%%"}) }},
		{"odd_audio", func(w *testLiveWire) { w.push(map[string]any{"type": "session.output_audio.delta", "delta": "AQ=="}) }},
		{"missing_transcript_timestamps", func(w *testLiveWire) {
			w.push(map[string]any{"type": "session.input_transcript.delta", "delta": "Deploy."})
		}},
		{"missing_delegation_offset", func(w *testLiveWire) {
			w.push(map[string]any{"type": "session.delegation.created", "delegation": map[string]any{"id": "bad", "type": "delegation", "target": "client"}})
		}},
		{"json", func(w *testLiveWire) { w.in <- []byte("{") }},
		{"eof", func(w *testLiveWire) { _ = w.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, audio := newTestLiveWire(), newTestLiveAudio()
			s := startTestLive(t, wire, audio)
			tc.send(wire)
			event := awaitLiveEvent(t, s, "error")
			if event.Err == nil || strings.Contains(event.Err.Error(), "test-secret") {
				t.Fatalf("missing or unsafe error: %v", event.Err)
			}
			select {
			case <-audio.done:
			case <-time.After(time.Second):
				t.Fatal("failure left microphone open")
			}
		})
	}
}

func TestLiveStartupFailureNeverOpensMicrophone(t *testing.T) {
	wire := newTestLiveWire()
	wire.push(map[string]any{"type": "error", "error": map[string]any{"message": "invalid key test-secret"}})
	_, err := startVoice(context.Background(), "test-secret", func(context.Context, string) (liveWire, error) { return wire, nil }, func() (VoiceAudio, error) { t.Fatal("microphone opened before session accepted"); return nil, nil })
	if err == nil || strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("unexpected startup error: %v", err)
	}
	select {
	case <-wire.done:
	default:
		t.Fatal("failed startup left connection open")
	}
}

func TestLiveContextCancellationUnblocksAppendAndAudio(t *testing.T) {
	wire, audio := newTestLiveWire(), newTestLiveAudio()
	wire.autoAck = false
	s := startTestLive(t, wire, audio)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- s.SendContext(ctx, "quiet context") }()
	awaitLiveSent(t, wire, "session.thinking.append")
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled append, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("append ignored cancellation")
	}
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("session cancellation did not release workers")
	}
}

func TestLiveCloseAcknowledgedAndBounded(t *testing.T) {
	for _, ack := range []bool{true, false} {
		wire, audio := newTestLiveWire(), newTestLiveAudio()
		wire.autoClose = ack
		s := startTestLive(t, wire, audio)
		s.closeWait = 30 * time.Millisecond
		closed := make(chan error, 1)
		go func() { closed <- s.Close() }()
		awaitLiveSent(t, wire, "session.close")
		select {
		case err := <-closed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatalf("Close hung (ack=%t)", ack)
		}
		select {
		case <-audio.done:
		default:
			t.Fatal("Close left microphone open")
		}
	}
}

func TestLiveTranscriptReconcilesDelayedFragmentsAndCorrections(t *testing.T) {
	var transcript liveTranscript
	now := time.Now()
	transcript.add("p1", "user", "Deploy ", 100, 200, now)
	transcript.delegate("first", 500, now)
	if _, ready := transcript.ready(now.Add(200*time.Millisecond), 300*time.Millisecond); ready {
		t.Fatal("executed raw fragment before reconciliation")
	}
	transcript.add("p2", "user", "the project.", 200, 400, now.Add(250*time.Millisecond))
	if transcript.add("p2", "user", "the project.", 200, 400, now) {
		t.Fatal("duplicate transcript event was added")
	}
	if _, ready := transcript.ready(now.Add(400*time.Millisecond), 300*time.Millisecond); ready {
		t.Fatal("did not allow delayed final fragment to settle")
	}
	event, ready := transcript.ready(now.Add(600*time.Millisecond), 300*time.Millisecond)
	if !ready || event.DelegationID != "first" || !strings.Contains(event.Text, "the project.") {
		t.Fatalf("missing complete delegated request: %#v", event)
	}
	if transcript.delegate("first", 500, now.Add(time.Second)) {
		t.Fatal("duplicate delegation accepted")
	}
	// A fragment arriving after its request completed must not cause a fresh
	// delegation to repeat that earlier action.
	transcript.add("late", "user", " please", 400, 450, now.Add(time.Second))
	transcript.delegate("duplicate-task", 600, now.Add(time.Second))
	if _, ready := transcript.ready(now.Add(4*time.Second), 300*time.Millisecond); ready {
		t.Fatal("re-executed already delivered speech")
	}
	transcript.add("p3", "user", "Actually cancel deployment.", 700, 900, now.Add(5*time.Second))
	transcript.delegate("correction", 1000, now.Add(5*time.Second))
	event, ready = transcript.ready(now.Add(6*time.Second), 300*time.Millisecond)
	if !ready || event.DelegationID != "correction" {
		t.Fatal("fresh correction was lost")
	}
	latest := strings.Split(event.Prompt, "Latest user request/correction")[1]
	if !strings.Contains(latest, "Actually cancel") || strings.Contains(latest, "Deploy ") || strings.Contains(latest, "please") {
		t.Fatalf("latest request includes already handled utterance: %s", latest)
	}
}

func TestLiveTranscriptOnlyDelegationsTriggerActions(t *testing.T) {
	var transcript liveTranscript
	now := time.Now()
	transcript.add("part", "user", "Read the project", 100, 200, now)
	if _, ready := transcript.ready(now.Add(time.Hour), 0); ready {
		t.Fatal("transcript alone triggered an action")
	}
	transcript.delegate("old", 300, now)
	transcript.add("correction", "user", "No, just list files", 400, 500, now)
	transcript.delegate("new", 600, now)
	event, ready := transcript.ready(now.Add(time.Second), 0)
	if !ready || event.DelegationID != "new" {
		t.Fatalf("latest pending delegation did not supersede old: %#v", event)
	}
}

func TestLiveBriefBoundedUTF8(t *testing.T) {
	for _, text := range []string{strings.Repeat("界", 1000), strings.Repeat("a", 1000)} {
		brief := liveBrief(text, 440)
		if !utf8.ValidString(brief) || len(brief) > 440 || !strings.Contains(brief, "Details are in the terminal") {
			t.Fatalf("unsafe append: %q", brief)
		}
	}
}

func TestLiveParentCancellationClosesSessionBeforeSocket(t *testing.T) {
	wire, audio := newTestLiveWire(), newTestLiveAudio()
	wire.push(map[string]any{"type": "session.started"})
	ctx, cancel := context.WithCancel(context.Background())
	session, err := startVoice(ctx, "test-secret", func(context.Context, string) (liveWire, error) { return wire, nil }, func() (VoiceAudio, error) { return audio, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = session.Close() })
	cancel()
	awaitLiveSent(t, wire, "session.close")
	select {
	case <-audio.done:
	default:
		t.Fatal("session.close sent before microphone stopped")
	}
	select {
	case <-session.(*liveSession).done:
	case <-time.After(time.Second):
		t.Fatal("parent cancellation failed to finish acknowledged close")
	}
}

func TestLiveCanceledStartupAndAudioFailureCloseSocket(t *testing.T) {
	t.Run("canceled_handshake", func(t *testing.T) {
		wire := newTestLiveWire()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := startVoice(ctx, "test-secret", func(context.Context, string) (liveWire, error) { return wire, nil }, func() (VoiceAudio, error) { return nil, errors.New("unexpected audio open") })
			result <- err
		}()
		awaitLiveSent(t, wire, "session.start")
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v instead of canceled handshake", err)
			}
		case <-time.After(time.Second):
			t.Fatal("startup ignored cancellation")
		}
	})
	t.Run("audio_open", func(t *testing.T) {
		wire := newTestLiveWire()
		wire.push(map[string]any{"type": "session.started"})
		_, err := startVoice(context.Background(), "test-secret", func(context.Context, string) (liveWire, error) { return wire, nil }, func() (VoiceAudio, error) { return nil, errors.New("no microphone") })
		if err == nil || !strings.Contains(err.Error(), "no microphone") {
			t.Fatalf("unexpected audio failure: %v", err)
		}
		select {
		case <-wire.done:
		default:
			t.Fatal("failed audio open leaked live socket")
		}
	})
}

func TestLiveCorrectionDropsStaleAndDuplicateReplies(t *testing.T) {
	wire, audio := newTestLiveWire(), newTestLiveAudio()
	s := startTestLive(t, wire, audio)
	wire.push(map[string]any{"type": "session.input_transcript.delta", "event_id": "first-speech", "delta": "Deploy.", "start_ms": 100, "end_ms": 200})
	wire.push(map[string]any{"type": "session.delegation.created", "offset_ms": 300, "delegation": map[string]any{"id": "first", "type": "delegation", "target": "client"}})
	awaitLiveEvent(t, s, "delegation")
	wire.push(map[string]any{"type": "session.input_transcript.delta", "event_id": "second-speech", "delta": "Actually cancel.", "start_ms": 400, "end_ms": 500})
	wire.push(map[string]any{"type": "session.delegation.created", "offset_ms": 600, "delegation": map[string]any{"id": "second", "type": "delegation", "target": "client"}})
	// The acknowledgment follows the correction on the receive queue, so the
	// old final result must already be invalid while new speech is settling.
	if err := s.SendContext(context.Background(), "The terminal is active."); err != nil {
		t.Fatal(err)
	}
	awaitLiveSent(t, wire, "session.thinking.append")
	if err := s.Reply(context.Background(), "first", "Obsolete result"); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-wire.out:
		t.Fatalf("stale result reached voice: %#v", event)
	default:
	}
	awaitLiveEvent(t, s, "delegation")
	if err := s.Reply(context.Background(), "second", "Canceled."); err != nil {
		t.Fatal(err)
	}
	awaitLiveSent(t, wire, "session.commentary.append")
	if err := s.Reply(context.Background(), "second", "Canceled."); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-wire.out:
		t.Fatalf("duplicate final result reached voice: %#v", event)
	default:
	}
}

func TestLiveWebSocketProtocolWithLocalServer(t *testing.T) {
	serverEvents := make(chan map[string]any, 16)
	serverErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/live/sessions" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer test-secret" {
			http.Error(w, "invalid test handshake", http.StatusBadRequest)
			return
		}
		websocket.Handler(func(conn *websocket.Conn) {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			for {
				var event map[string]any
				if err := websocket.JSON.Receive(conn, &event); err != nil {
					serverErrors <- err
					return
				}
				serverEvents <- event
				switch event["type"] {
				case "session.start":
					_ = websocket.JSON.Send(conn, map[string]any{"type": "session.started"})
				case "session.input_audio.append":
					_ = websocket.JSON.Send(conn, map[string]any{"type": "session.delegation.created", "offset_ms": 500, "delegation": map[string]any{"id": "live-opaque", "type": "delegation", "target": "client"}})
					_ = websocket.JSON.Send(conn, map[string]any{"type": "session.input_transcript.delta", "event_id": "late-transcript", "start_ms": 100, "end_ms": 400, "delta": "List the devices."})
				case "session.commentary.append":
					_ = websocket.JSON.Send(conn, map[string]any{"type": "session.commentary.appended", "client_event_id": event["event_id"]})
				case "session.close":
					_ = websocket.JSON.Send(conn, map[string]any{"type": "session.closed"})
					return
				}
			}
		}).ServeHTTP(w, r)
	}))
	defer server.Close()
	audio := newTestLiveAudio()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, err := startVoice(ctx, "test-secret", func(ctx context.Context, key string) (liveWire, error) {
		config, err := websocket.NewConfig("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/live/sessions", server.URL)
		if err != nil {
			return nil, err
		}
		config.Header.Set("Authorization", "Bearer "+key)
		conn, err := config.DialContext(ctx)
		if err != nil {
			return nil, err
		}
		return &liveSocket{conn: conn, gate: make(chan struct{}, 1)}, nil
	}, func() (VoiceAudio, error) { return audio, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	audio.input <- []byte{1, 0, 2, 0}
	event := awaitLiveEvent(t, session, "delegation")
	if event.Text != "List the devices." || !strings.Contains(event.Prompt, "start_ms") {
		t.Fatalf("unexpected reconciled delegation: %#v", event)
	}
	if err := session.Reply(ctx, event.DelegationID, "Two devices are available."); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"session.start", "session.input_audio.append", "session.commentary.append", "session.close"} {
		select {
		case event := <-serverEvents:
			if event["type"] != want {
				t.Fatalf("expected %s, got %#v", want, event)
			}
		case <-time.After(time.Second):
			t.Fatalf("server did not receive %s", want)
		}
	}
	select {
	case err := <-serverErrors:
		t.Fatalf("server protocol failed: %v", err)
	default:
	}
}

func TestLiveQuietContextPreservesLatestConversationInBoundedChunks(t *testing.T) {
	wire, audio := newTestLiveWire(), newTestLiveAudio()
	s := startTestLive(t, wire, audio)
	awaitLiveSent(t, wire, "session.start")
	contextText := "Workspace: /test/project. Terminal approval remains required.\n" + strings.Repeat("Earlier discussion about the device 界.\n", 600) + "\nLatest request: inspect the blue Jetson."
	if err := s.SendContext(context.Background(), contextText); err != nil {
		t.Fatal(err)
	}
	var contents []string
	var bytes int
	for {
		select {
		case event := <-wire.out:
			content, _ := event["content"].(string)
			if event["type"] != "session.thinking.append" || event["delegation_id"] != nil || len(content) > 440 || !utf8.ValidString(content) {
				t.Fatalf("invalid quiet context append: %#v", event)
			}
			if strings.Contains(content, "Details are in the terminal") {
				t.Fatal("quiet context was truncated as if it were a spoken result")
			}
			contents = append(contents, content)
			bytes += len(content)
		default:
			combined := strings.Join(contents, "\n")
			if len(contents) < 2 || bytes > 8*1024 || !strings.Contains(combined, "Workspace: /test/project") || !strings.Contains(combined, "Latest request: inspect the blue Jetson.") {
				t.Fatalf("context lost workspace/latest request or exceeded bound: chunks=%d bytes=%d", len(contents), bytes)
			}
			return
		}
	}
}

func TestLiveQuietContextStopsChunksOnCancellationOrRejection(t *testing.T) {
	for _, rejection := range []bool{false, true} {
		wire, audio := newTestLiveWire(), newTestLiveAudio()
		wire.autoAck = false
		s := startTestLive(t, wire, audio)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.SendContext(ctx, strings.Repeat("Long quiet context. ", 200)) }()
		first := awaitLiveSent(t, wire, "session.thinking.append")
		if rejection {
			wire.push(map[string]any{"type": "error", "error": map[string]any{"message": "Context rejected", "client_event_id": first["event_id"]}})
		} else {
			cancel()
		}
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("canceled/rejected context succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("context ignored cancellation/rejection")
		}
		cancel()
		select {
		case event := <-wire.out:
			t.Fatalf("sent another chunk after cancellation/rejection: %#v", event)
		default:
		}
	}
}

func TestLiveOnlyAcceptedFreshCommentaryResumesInterruptedPlayback(t *testing.T) {
	for _, mode := range []string{"accepted", "canceled", "rejected", "newer_interrupt", "newer_delegation"} {
		t.Run(mode, func(t *testing.T) {
			wire, audio := newTestLiveWire(), newTestLiveAudio()
			wire.autoAck = false
			s := startTestLive(t, wire, audio)
			interrupt := func() {
				t.Helper()
				done := make(chan error, 1)
				go func() { done <- s.Interrupt(context.Background()) }()
				event := awaitLiveSent(t, wire, "session.instructions.append")
				wire.push(map[string]any{"type": "session.instructions.appended", "client_event_id": event["event_id"]})
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			interrupt()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- s.Reply(ctx, "", "Please approve the new action in the terminal.") }()
			event := awaitLiveSent(t, wire, "session.commentary.append")
			s.mu.Lock()
			beforeACK := s.interrupted
			s.mu.Unlock()
			if !beforeACK {
				t.Fatal("commentary reopened playback before acceptance")
			}
			switch mode {
			case "canceled":
				cancel()
			case "rejected":
				wire.push(map[string]any{"type": "error", "error": map[string]any{"message": "Rejected commentary", "client_event_id": event["event_id"]}})
			case "newer_interrupt":
				interrupt()
			case "newer_delegation":
				wire.push(map[string]any{"type": "session.delegation.created", "offset_ms": 500, "delegation": map[string]any{"id": "new-task", "type": "delegation", "target": "client"}})
			}
			if mode != "rejected" {
				wire.push(map[string]any{"type": "session.commentary.appended", "client_event_id": event["event_id"]})
			}
			select {
			case err := <-done:
				if (mode == "canceled" || mode == "rejected") != (err != nil) {
					t.Fatalf("unexpected reply result: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("commentary did not complete")
			}
			s.mu.Lock()
			muted := s.interrupted
			s.mu.Unlock()
			if muted != (mode != "accepted") {
				t.Fatalf("wrong playback state after %s commentary: muted=%t", mode, muted)
			}
			if mode == "accepted" {
				wire.push(map[string]any{"type": "session.output_audio.delta", "delta": "AQACAA=="})
				select {
				case <-audio.writes:
				case <-time.After(time.Second):
					t.Fatal("fresh speech was discarded after accepted commentary")
				}
			}
		})
	}
}
