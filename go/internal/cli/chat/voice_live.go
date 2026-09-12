package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/websocket"
)

// VoiceEvent carries display transcripts and complete, reconciled delegations.
// Only a delegation event authorizes the TUI to start an agent turn.
type VoiceEvent struct {
	Type         string
	Text         string
	Prompt       string // Optional timestamped backend context; Text stays readable.
	DelegationID string
	Err          error
}

// VoiceSession keeps listening while the existing agent runs or awaits approval.
type VoiceSession interface {
	Events() <-chan VoiceEvent
	SendContext(context.Context, string) error
	Reply(context.Context, string, string) error
	Interrupt(context.Context) error
	Close() error
}

const liveEndpoint = "wss://api.openai.com/v1/live/sessions"
const liveModel = "gpt-live-1"

const liveInstructions = `You are Wendy's quiet voice interface to a coding and hardware agent. Keep listening; do not greet first. Speak briefly only for clarification, an explicit approval prompt, a blocker, or verified completion. Stay silent while the backend thinks or uses tools. Do not narrate plans, code, logs, or routine progress.
Backchannel policy: No routine listening sounds or filler.
Interruption policy: Stop speaking when the user interrupts and listen.
Delegation policy: The backend can inspect and edit the local project, build and test code, and discover, control, deploy to, and debug Wendy devices. Delegate requests for those tasks or careful reasoning. Delegate corrections or cancellation of active work. Do not delegate greetings or requests to repeat a verified result. If a request is unclear, ask one short clarification. Delegate before a result-dependent answer; never invent results. Agent actions requiring approval are approved only through terminal y/n; spoken yes does not approve them. Context appended as thinking is background information and needs no spoken acknowledgment. Announce only the concise final result supplied by the backend.`

type liveWire interface {
	Send(context.Context, any) error
	Receive(any) error
	Close() error
}

type liveSocket struct {
	conn *websocket.Conn
	gate chan struct{}
}

func dialLive(ctx context.Context, key string) (liveWire, error) {
	config, err := websocket.NewConfig(liveEndpoint, "https://api.openai.com")
	if err != nil {
		return nil, err
	}
	config.Header = http.Header{"Authorization": {"Bearer " + key}}
	conn, err := config.DialContext(ctx)
	if err != nil {
		return nil, err
	}
	conn.MaxPayloadBytes = 2 << 20
	return &liveSocket{conn: conn, gate: make(chan struct{}, 1)}, nil
}

func (w *liveSocket) Send(ctx context.Context, value any) error {
	select {
	case w.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-w.gate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := w.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	// Closing the socket also releases a write blocked inside the TLS transport.
	stop := context.AfterFunc(ctx, func() { _ = w.conn.Close() })
	defer stop()
	return websocket.JSON.Send(w.conn, value)
}

func (w *liveSocket) Receive(value any) error { return websocket.JSON.Receive(w.conn, value) }
func (w *liveSocket) Close() error            { return w.conn.Close() }

type liveServerEvent struct {
	Type          string   `json:"type"`
	EventID       string   `json:"event_id"`
	ClientEventID string   `json:"client_event_id"`
	Delta         string   `json:"delta"`
	StartMS       *float64 `json:"start_ms"`
	EndMS         *float64 `json:"end_ms"`
	OffsetMS      *float64 `json:"offset_ms"`
	Delegation    struct {
		ID     string `json:"id"`
		Type   string `json:"type"`
		Target string `json:"target"`
	} `json:"delegation"`
	Error struct {
		Type          string `json:"type"`
		Code          string `json:"code"`
		Message       string `json:"message"`
		ClientEventID string `json:"client_event_id"`
	} `json:"error"`
}

type livePlayback struct {
	data       []byte
	generation uint64
}

type liveSession struct {
	ctx          context.Context
	cancel       context.CancelFunc
	wire         liveWire
	audio        VoiceAudio
	key          string
	events       chan VoiceEvent
	playback     chan livePlayback
	done         chan struct{}
	remoteClosed chan struct{}
	wg           sync.WaitGroup
	closeOnce    sync.Once
	closing      atomic.Bool
	sequence     atomic.Uint64
	generation   atomic.Uint64

	mu          sync.Mutex
	transcript  liveTranscript
	activeID    string
	replied     map[string]bool
	acks        map[string]chan error
	interrupted bool
	interruptID uint64
	lastOutput  time.Time
	interruptAt time.Time
	settle      time.Duration
	closeWait   time.Duration
}

// StartVoice opens GPT-Live's native session protocol, with client delegation so
// the configured agent provider and its existing approval policy remain in use.
func StartVoice(ctx context.Context, key string) (VoiceSession, error) {
	return startVoice(ctx, key, dialLive, openVoiceAudio)
}

func startVoice(ctx context.Context, key string, dial func(context.Context, string) (liveWire, error), openAudio func() (VoiceAudio, error)) (VoiceSession, error) {
	key = strings.TrimSpace(key)
	if key == "" || strings.IndexFunc(key, unicode.IsControl) >= 0 {
		return nil, errors.New("voice requires a valid OpenAI API key; run wendy chat --voice to configure it")
	}
	startup, cancelStartup := context.WithTimeout(ctx, 15*time.Second)
	defer cancelStartup()
	wire, err := dial(startup, key)
	if err != nil {
		return nil, liveError(key, "connect to GPT-Live", err)
	}
	stop := context.AfterFunc(startup, func() { _ = wire.Close() })
	defer stop()
	started := false
	defer func() {
		if !started {
			_ = wire.Close()
		}
	}()
	request := map[string]any{
		"type": "session.start", "event_id": "wendy-start",
		"session": map[string]any{
			"model": liveModel, "instructions": liveInstructions, "store": false,
			"audio":      map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}, "output": map[string]any{"voice": "marin"}},
			"delegation": map[string]any{"type": "client"},
		},
	}
	if err := wire.Send(startup, request); err != nil {
		return nil, liveError(key, "start GPT-Live", err)
	}
	var hello liveServerEvent
	if err := wire.Receive(&hello); err != nil {
		if startup.Err() != nil {
			err = startup.Err()
		}
		return nil, liveError(key, "start GPT-Live", err)
	}
	if hello.Type == "error" {
		return nil, liveError(key, "GPT-Live", errors.New(hello.Error.Message))
	}
	if hello.Type != "session.started" {
		return nil, errors.New("GPT-Live did not acknowledge session.start")
	}
	if err := startup.Err(); err != nil {
		return nil, err
	}
	audio, err := openAudio()
	if err != nil {
		return nil, fmt.Errorf("open voice audio: %w", err)
	}
	if !stop() || startup.Err() != nil {
		_ = audio.Close()
		return nil, startup.Err()
	}
	// Parent cancellation must stop microphone capture immediately while still
	// allowing a bounded session.close handshake on the live connection.
	sessionCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &liveSession{
		ctx: sessionCtx, cancel: cancel, wire: wire, audio: audio, key: key,
		events: make(chan VoiceEvent, 256), playback: make(chan livePlayback, 8),
		done: make(chan struct{}), remoteClosed: make(chan struct{}),
		replied: make(map[string]bool), acks: make(map[string]chan error),
		settle: 300 * time.Millisecond, closeWait: 3 * time.Second,
	}
	s.events <- VoiceEvent{Type: "ready"}
	s.wg.Add(4)
	stopParent := context.AfterFunc(ctx, func() { _ = s.Close() })
	go s.capture()
	go s.play()
	go s.receive()
	go s.reconcile()
	go func() {
		defer stopParent()
		<-s.ctx.Done()
		_ = s.wire.Close()
		_ = s.audio.Close()
		s.wg.Wait()
		select {
		case s.events <- VoiceEvent{Type: "done"}:
		default:
		}
		close(s.events)
		close(s.done)
	}()
	started = true
	return s, nil
}

func liveError(key, action string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	provider := httpProvider{config: Config{APIKey: key}}
	return fmt.Errorf("%s: %s", action, provider.safeError(err.Error()))
}

func (s *liveSession) Events() <-chan VoiceEvent { return s.events }

func (s *liveSession) publish(event VoiceEvent) {
	select {
	case s.events <- event:
	default:
		// Losing a display delta is harmless. Losing an action would not be;
		// stop the session rather than silently dropping a delegated request.
		if event.Type == "delegation" {
			s.cancel()
		}
	}
}

func (s *liveSession) fail(err error) {
	if s.ctx.Err() == nil && !s.closing.Load() {
		s.publish(VoiceEvent{Type: "error", Err: liveError(s.key, "voice", err)})
	}
	s.cancel()
}

func (s *liveSession) send(ctx context.Context, value any) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	if err := s.wire.Send(bounded, value); err != nil {
		return liveError(s.key, "send voice event", err)
	}
	return nil
}

func (s *liveSession) capture() {
	defer s.wg.Done()
	buffer := make([]byte, 4801)
	remaining := 0
	for s.ctx.Err() == nil && !s.closing.Load() {
		n, err := s.audio.Read(buffer[remaining:4800])
		if n > 0 {
			n += remaining
			complete := n - n%2
			if complete > 0 && !s.closing.Load() {
				if sendErr := s.send(s.ctx, map[string]any{"type": "session.input_audio.append", "audio": base64.StdEncoding.EncodeToString(buffer[:complete])}); sendErr != nil {
					s.fail(sendErr)
					return
				}
			}
			remaining = n - complete
			if remaining != 0 {
				buffer[0] = buffer[complete]
			}
		}
		if err != nil {
			if !s.closing.Load() && s.ctx.Err() == nil {
				s.fail(fmt.Errorf("microphone stopped: %w", err))
			}
			return
		}
	}
}

func (s *liveSession) play() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case packet := <-s.playback:
			if packet.generation != s.generation.Load() {
				continue
			}
			n, err := s.audio.Write(packet.data)
			if err == nil && n != len(packet.data) {
				err = io.ErrShortWrite
			}
			if packet.generation != s.generation.Load() {
				_ = s.audio.Flush()
			}
			if err != nil && !errors.Is(err, ErrVoicePlaybackInterrupted) && s.ctx.Err() == nil && !s.closing.Load() {
				s.fail(fmt.Errorf("speaker stopped: %w", err))
				return
			}
		}
	}
}

func (s *liveSession) receive() {
	defer s.wg.Done()
	for {
		var event liveServerEvent
		if err := s.wire.Receive(&event); err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("GPT-Live disconnected before session.closed")
			}
			s.fail(err)
			return
		}
		switch event.Type {
		case "session.output_audio.delta":
			if len(event.Delta) > 128*1024 {
				s.fail(errors.New("GPT-Live audio frame exceeds playback limit"))
				return
			}
			data, err := base64.StdEncoding.DecodeString(event.Delta)
			if err != nil || len(data)%2 != 0 {
				s.fail(errors.New("GPT-Live sent malformed PCM audio"))
				return
			}
			now := time.Now()
			s.mu.Lock()
			// Live has no response.cancel/audio-done event. An explicit local
			// interruption discards the current burst until a quiet audio gap.
			if s.interrupted && now.Sub(s.lastOutput) > 350*time.Millisecond && now.Sub(s.interruptAt) > 500*time.Millisecond {
				s.interrupted = false
			}
			s.lastOutput = now
			drop := s.interrupted
			s.mu.Unlock()
			if drop || len(data) == 0 || s.closing.Load() {
				continue
			}
			packet := livePlayback{data: data, generation: s.generation.Load()}
			select {
			case s.playback <- packet:
			default:
				// Bound latency if the device cannot keep up. Flush unblocks a
				// pending write; transcript/control handling never waits on audio.
				s.generation.Add(1)
				_ = s.audio.Flush()
			}
		case "session.input_transcript.delta", "session.output_transcript.delta":
			if event.StartMS == nil || event.EndMS == nil || *event.StartMS < 0 || *event.EndMS < *event.StartMS || len(event.Delta) > 16*1024 {
				s.fail(errors.New("GPT-Live sent invalid transcript timestamps"))
				return
			}
			speaker := "user"
			kind := "input"
			if event.Type == "session.output_transcript.delta" {
				speaker, kind = "assistant", "output"
			}
			s.mu.Lock()
			added := s.transcript.add(event.EventID, speaker, event.Delta, *event.StartMS, *event.EndMS, time.Now())
			s.mu.Unlock()
			if added {
				s.publish(VoiceEvent{Type: kind, Text: event.Delta})
			}
		case "session.delegation.created":
			if event.Delegation.ID == "" || event.Delegation.Type != "delegation" || event.Delegation.Target != "client" || event.OffsetMS == nil || *event.OffsetMS < 0 {
				s.fail(errors.New("GPT-Live sent an invalid client delegation"))
				return
			}
			s.mu.Lock()
			if s.transcript.delegate(event.Delegation.ID, *event.OffsetMS, time.Now()) {
				// Invalidate an old result as soon as a correction is delegated,
				// before its delayed transcript has finished reconciling.
				s.activeID = event.Delegation.ID
			}
			s.mu.Unlock()
		case "session.thinking.appended", "session.commentary.appended", "session.instructions.appended":
			s.ack(event.ClientEventID, nil)
		case "error":
			err := liveError(s.key, "GPT-Live", errors.New(event.Error.Message))
			s.ack(event.Error.ClientEventID, err)
			s.fail(err)
			return
		case "session.closed":
			close(s.remoteClosed)
			s.cancel()
			return
		case "":
			s.fail(errors.New("GPT-Live sent an event without a type"))
			return
		default:
			// Allow additive protocol events (usage and diagnostics).
		}
	}
}

func (s *liveSession) ack(id string, err error) {
	s.mu.Lock()
	waiter := s.acks[id]
	delete(s.acks, id)
	s.mu.Unlock()
	if waiter != nil {
		waiter <- err
	}
}

func (s *liveSession) append(ctx context.Context, kind, id, content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	// A Live context append is limited to 500 tokens. Byte-bounded UTF-8 text
	// stays below that limit even for code and non-English speech, without a
	// model-specific tokenizer. Large artifacts remain in the terminal.
	content = liveBrief(content, 440)
	eventID := fmt.Sprintf("wendy-%d", s.sequence.Add(1))
	var delegationID any
	if id != "" {
		delegationID = id
	}
	waiter := make(chan error, 1)
	s.mu.Lock()
	s.acks[eventID] = waiter
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.acks, eventID)
		s.mu.Unlock()
	}()
	if err := s.send(ctx, map[string]any{"type": "session." + kind + ".append", "event_id": eventID, "delegation_id": delegationID, "content": content}); err != nil {
		return err
	}
	// Acknowledgment means the context was injected, not that it was spoken.
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case err := <-waiter:
		return err
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("GPT-Live did not acknowledge the voice context")
	}
}

func liveBrief(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	const suffix = "… Details are in the terminal."
	end := limit - len(suffix)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return strings.TrimSpace(text[:end]) + suffix
}

func (s *liveSession) SendContext(ctx context.Context, text string) error {
	text = strings.ToValidUTF8(text, "�")
	// Preserve the workspace prefix and the latest exchanges if a long text
	// conversation is attached. Each ordered append obeys Live's 500-token
	// limit; none asks the voice model to acknowledge the context aloud.
	const limit = 8 * 1024
	if len(text) > limit {
		const omitted = "\n[Earlier context omitted.]\n"
		headEnd := 1024
		for !utf8.RuneStart(text[headEnd]) {
			headEnd--
		}
		tailStart := len(text) - (limit - headEnd - len(omitted))
		for tailStart < len(text) && !utf8.RuneStart(text[tailStart]) {
			tailStart++
		}
		text = text[:headEnd] + omitted + text[tailStart:]
	}
	for text != "" {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(440, len(text))
		if end < len(text) {
			for !utf8.RuneStart(text[end]) {
				end--
			}
			// Prefer whole words/lines while keeping chunks reasonably full.
			if split := strings.LastIndexAny(text[:end], " \n\t"); split >= end/2 {
				end = split + 1
			}
		}
		if err := s.append(ctx, "thinking", "", text[:end]); err != nil {
			return err
		}
		text = text[end:]
	}
	return nil
}

func (s *liveSession) Reply(ctx context.Context, id, text string) error {
	s.mu.Lock()
	interruptID, activeID := s.interruptID, s.activeID
	if id != "" {
		if id != s.activeID || s.replied[id] {
			s.mu.Unlock()
			return nil // A newer spoken correction supersedes the old result.
		}
		s.replied[id] = true
	}
	s.mu.Unlock()
	if err := s.append(ctx, "commentary", id, text); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// An accepted fresh result or approval question begins new speech even
	// when it immediately follows the interrupted audio burst. A delayed ACK
	// for superseded speech must not undo a newer interruption or correction.
	s.mu.Lock()
	if strings.TrimSpace(text) != "" && s.interruptID == interruptID && s.activeID == activeID && s.ctx.Err() == nil && !s.closing.Load() {
		s.interrupted = false
	}
	s.mu.Unlock()
	return nil
}

func (s *liveSession) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	s.interrupted = true
	s.interruptID++
	s.interruptAt = time.Now()
	s.lastOutput = s.interruptAt
	s.mu.Unlock()
	s.generation.Add(1)
	if err := s.audio.Flush(); err != nil {
		return err
	}
	return s.append(ctx, "instructions", "", "Stop speaking now and listen. Remain silent until the user requests speech or the backend supplies a new final result or approval question. This instruction stops speech only; it does not cancel any backend action.")
}

func (s *liveSession) Close() error {
	s.closeOnce.Do(func() {
		s.closing.Store(true)
		_ = s.audio.Close()
		if s.ctx.Err() == nil {
			ctx, cancel := context.WithTimeout(context.Background(), s.closeWait)
			_ = s.send(ctx, map[string]any{"type": "session.close", "event_id": fmt.Sprintf("wendy-%d", s.sequence.Add(1))})
			select {
			case <-s.remoteClosed:
			case <-s.ctx.Done():
			case <-ctx.Done():
			}
			cancel()
		}
		s.cancel()
	})
	<-s.done
	return nil
}

type liveTranscriptPart struct {
	Speaker string  `json:"speaker"`
	Text    string  `json:"text"`
	StartMS float64 `json:"start_ms"`
	EndMS   float64 `json:"end_ms"`
	seq     uint64
}

type liveDelegation struct {
	id      string
	offset  float64
	created time.Time
}

// Live supplies timestamped fragments, not completed user turns or task text.
// Reconcile around delegation offsets and allow delayed transcripts to arrive.
type liveTranscript struct {
	parts        []liveTranscriptPart
	events       map[string]bool
	delegations  map[string]bool
	sequence     uint64
	delivered    uint64
	deliveredMS  float64
	pending      *liveDelegation
	lastFragment time.Time
	bytes        int
}

func (t *liveTranscript) add(id, speaker, text string, start, end float64, now time.Time) bool {
	if text == "" {
		return false
	}
	if t.events == nil {
		t.events = make(map[string]bool)
	}
	if id != "" {
		if t.events[id] {
			return false
		}
		if len(t.events) >= 8192 {
			t.events = make(map[string]bool)
		}
		t.events[id] = true
	}
	t.sequence++
	t.parts = append(t.parts, liveTranscriptPart{Speaker: speaker, Text: text, StartMS: start, EndMS: end, seq: t.sequence})
	t.bytes += len(text)
	if speaker == "user" {
		t.lastFragment = now
	}
	for t.bytes > 16*1024 && len(t.parts) > 1 {
		t.bytes -= len(t.parts[0].Text)
		t.parts = t.parts[1:]
	}
	return true
}

func (t *liveTranscript) delegate(id string, offset float64, now time.Time) bool {
	if t.delegations == nil {
		t.delegations = make(map[string]bool)
	}
	if t.delegations[id] {
		return false
	}
	// Bound state across arbitrarily long microphone sessions. Transcript
	// offsets still prevent an evicted old ID from repeating completed work.
	if len(t.delegations) >= 8192 {
		t.delegations = make(map[string]bool)
	}
	t.delegations[id] = true
	if offset > 0 && offset <= t.deliveredMS {
		return false
	}
	t.pending = &liveDelegation{id: id, offset: offset, created: now}
	return true
}

func (t *liveTranscript) ready(now time.Time, settle time.Duration) (VoiceEvent, bool) {
	pending := t.pending
	if pending == nil || now.Sub(pending.created) < settle || (now.Sub(t.lastFragment) < settle && now.Sub(pending.created) < 2*time.Second) {
		return VoiceEvent{}, false
	}
	var recent, fresh []liveTranscriptPart
	var delivered uint64
	for _, part := range t.parts {
		if pending.offset > 0 && part.StartMS > pending.offset {
			continue
		}
		if part.Speaker == "user" && part.seq > t.delivered && (t.deliveredMS == 0 || part.EndMS > t.deliveredMS) {
			fresh = append(fresh, part)
			if part.seq > delivered {
				delivered = part.seq
			}
		} else {
			recent = append(recent, part)
		}
	}
	if len(fresh) == 0 {
		if now.Sub(pending.created) >= 2*time.Second {
			t.pending = nil // Never execute a task from missing or repeated speech.
		}
		return VoiceEvent{}, false
	}
	t.pending = nil
	t.delivered = delivered
	t.deliveredMS = pending.offset
	// Keep speaker boundaries and timestamps explicit; speech is user data,
	// never instructions for the voice transport or a terminal approval.
	history, _ := json.Marshal(recent)
	request, _ := json.Marshal(fresh)
	var spoken strings.Builder
	for _, part := range fresh {
		spoken.WriteString(part.Text)
	}
	return VoiceEvent{Type: "delegation", DelegationID: pending.id, Text: strings.TrimSpace(spoken.String()), Prompt: "The user delegated a spoken request to the Wendy agent. Act on the latest request or correction below; earlier requests may already be handled. Spoken agreement does not replace terminal approval. Transcription can be imperfect; clarify ambiguity before consequential actions.\nEarlier voice conversation (timestamped transcript data):\n" + string(history) + "\nLatest user request/correction (timestamped transcript data):\n" + string(request)}, true
}

func (s *liveSession) reconcile() {
	defer s.wg.Done()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			s.mu.Lock()
			event, ready := s.transcript.ready(now, s.settle)
			if ready {
				s.activeID = event.DelegationID
			}
			s.mu.Unlock()
			if ready {
				s.publish(event)
			}
		}
	}
}
