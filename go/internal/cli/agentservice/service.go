package agentservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/a2a"
	"golang.org/x/time/rate"
)

type Runner func(context.Context, string) (string, error)
type record struct {
	DelegationDepth int       `json:"delegation_depth,omitempty"`
	Task            a2a.Task  `json:"task"`
	Prompt          string    `json:"prompt"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
}
type receipt struct {
	Key    string `json:"key"`
	Hash   string `json:"hash"`
	TaskID string `json:"task_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}
type triggerState struct {
	Count int       `json:"count"`
	Last  time.Time `json:"last"`
	Fired time.Time `json:"fired"`
}
type database struct {
	Version    int                     `json:"version"`
	ConfigHash string                  `json:"config_hash"`
	Records    []record                `json:"records"`
	Receipts   []receipt               `json:"receipts"`
	Triggers   map[string]triggerState `json:"triggers"`
}
type Service struct {
	eventLimiter *rate.Limiter
	storageErr   error
	config       Config
	file         string
	lock         *flock.Flock
	mu           sync.Mutex
	db           database
	wake         chan struct{}
	changed      chan struct{}
	active       map[string]context.CancelFunc
	runner       Runner
	now          func() time.Time
}

var ErrStorage = errors.New("agent persistence failed; restart after repairing storage")

var ErrNotFound = errors.New("task not found")
var ErrConflict = errors.New("request ID was already used with different content or its retained task expired")
var ErrEventRate = errors.New("sensor event rate exceeded; retry later")
var ErrFull = errors.New("agent queue is full")
var ErrTerminal = errors.New("task is already terminal")

func Open(c Config, directory string, runner Runner) (*Service, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if runner == nil {
		return nil, errors.New("agent runner is required")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(directory, 0700); err != nil {
			return nil, fmt.Errorf("making agent state directory private: %w", err)
		}
	}
	lock := flock.New(filepath.Join(directory, "service.lock"))
	ok, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("another agent service owns this state directory")
	}
	s := &Service{eventLimiter: rate.NewLimiter(10, 10), config: c, file: filepath.Join(directory, "state.json"), lock: lock, wake: make(chan struct{}, 1), changed: make(chan struct{}), active: map[string]context.CancelFunc{}, runner: runner, now: time.Now}
	data, _ := json.Marshal(c)
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	s.db = database{Version: 1, ConfigHash: hash, Triggers: map[string]triggerState{}}
	data, err = os.ReadFile(s.file)
	if err == nil {
		if len(data) > 64<<20 {
			err = errors.New("agent state exceeds 64 MiB")
		} else {
			err = json.Unmarshal(data, &s.db)
		}
		if err == nil && (s.db.Version != 1 || s.db.ConfigHash != hash || s.db.Triggers == nil) {
			err = errors.New("agent state belongs to a different configuration; use a new state directory")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	// A crash can leave an external effect with no recorded result. Never replay
	// a working task automatically; queued, unstarted tasks remain resumable.
	for i := range s.db.Records {
		r := &s.db.Records[i]
		if r.Task.Status.State == a2a.Working {
			s.finish(r, a2a.Failed, "Service restarted during execution; effects may have completed. Inspect before retrying.")
		}
	}
	if err := s.save(s.db); err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	return s, nil
}

// Close must follow termination of Run and HTTP handlers.
func (s *Service) Close() error { return s.lock.Unlock() }
func (s *Service) save(db database) error {
	data, err := json.Marshal(db)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.file), ".state-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, s.file); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.file))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// change runs under mu and publishes only after a durable write succeeds.
func (s *Service) change(fn func(*database) error) error {
	if s.storageErr != nil {
		return s.storageErr
	}
	data, _ := json.Marshal(s.db)
	var next database
	if err := json.Unmarshal(data, &next); err != nil {
		return err
	}
	if err := fn(&next); err != nil {
		return err
	}
	if err := s.save(next); err != nil {
		s.storageErr = fmt.Errorf("%w: %v", ErrStorage, err)
		for _, cancel := range s.active {
			cancel()
		}
		close(s.changed)
		s.changed = make(chan struct{})
		select {
		case s.wake <- struct{}{}:
		default:
		}
		return s.storageErr
	}
	s.db = next
	close(s.changed)
	s.changed = make(chan struct{})
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}
func digest(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func find(db *database, id string) *record {
	for i := range db.Records {
		if db.Records[i].Task.ID == id {
			return &db.Records[i]
		}
	}
	return nil
}
func (s *Service) finish(r *record, state, text string) {
	if len(text) > 32000 {
		text = strings.ToValidUTF8(text[:32000], "") + "\n[Truncated.]"
	}
	r.Task.Status = a2a.Status{State: state, Timestamp: s.now().UTC()}
	if state == a2a.Completed {
		r.Task.Artifacts = []a2a.Artifact{{ID: uuid.NewString(), Name: "result", Parts: []a2a.Part{{Text: text}}}}
	} else {
		r.Task.Status.Message = &a2a.Message{MessageID: uuid.NewString(), Role: "ROLE_AGENT", Parts: []a2a.Part{{Text: text}}}
	}
}
func (s *Service) add(db *database, prompt, contextID string, expires time.Time) (a2a.Task, error) {
	pending := 0
	for _, r := range db.Records {
		if !a2a.Terminal(r.Task.Status.State) {
			pending++
		}
	}
	if pending >= 64 {
		return a2a.Task{}, ErrFull
	}
	if len(db.Records) >= s.config.MaxTasks {
		removed := false
		for i, r := range db.Records {
			if a2a.Terminal(r.Task.Status.State) && s.active[r.Task.ID] == nil {
				db.Records = append(db.Records[:i], db.Records[i+1:]...)
				removed = true
				break
			}
		}
		if !removed {
			return a2a.Task{}, ErrFull
		}
	}
	if contextID == "" {
		contextID = uuid.NewString()
	}
	task := a2a.Task{ID: uuid.NewString(), ContextID: contextID, Status: a2a.Status{State: a2a.Submitted, Timestamp: s.now().UTC()}}
	db.Records = append(db.Records, record{Task: task, Prompt: prompt, ExpiresAt: expires})
	return task, nil
}
func receiptFor(db *database, key, hash string) (*receipt, error) {
	for i := range db.Receipts {
		r := &db.Receipts[i]
		if r.Key == key {
			if r.Hash != hash {
				return nil, ErrConflict
			}
			if r.TaskID != "" && find(db, r.TaskID) == nil {
				return nil, ErrConflict
			}
			return r, nil
		}
	}
	return nil, nil
}
func remember(db *database, r receipt) {
	db.Receipts = append(db.Receipts, r)
	if len(db.Receipts) > 4096 {
		db.Receipts = db.Receipts[len(db.Receipts)-4096:]
	}
}
func (s *Service) Submit(req a2a.SendRequest) (a2a.Task, error) {
	depth, err := a2a.RequestDepth(req.Metadata)
	if err != nil {
		return a2a.Task{}, err
	}
	m := req.Message
	if m.Role != "ROLE_USER" || strings.TrimSpace(m.MessageID) == "" || len(m.MessageID) > 256 || len(m.ContextID) > 256 || m.TaskID != "" || len(m.Parts) == 0 || len(m.Parts) > 32 {
		return a2a.Task{}, errors.New("send requires a user text message, messageId, and no taskId; task continuation is unsupported")
	}
	var parts []string
	for _, p := range m.Parts {
		if p.Text == "" {
			return a2a.Task{}, errors.New("only text parts are supported")
		}
		parts = append(parts, p.Text)
	}
	prompt := strings.Join(parts, "\n")
	if len(prompt) > 16000 || strings.TrimSpace(prompt) == "" {
		return a2a.Task{}, errors.New("message text must be 1-16000 bytes")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := "message:" + m.MessageID
	hash := digest(struct {
		Message a2a.Message
		Depth   int
	}{m, depth})
	prior, err := receiptFor(&s.db, key, hash)
	if err != nil {
		return a2a.Task{}, err
	}
	if prior != nil {
		return cloneTask(find(&s.db, prior.TaskID).Task), nil
	}
	var task a2a.Task
	err = s.change(func(db *database) error {
		var err error
		task, err = s.add(db, prompt, m.ContextID, time.Time{})
		if err != nil {
			return err
		}
		find(db, task.ID).DelegationDepth = depth
		remember(db, receipt{Key: key, Hash: hash, TaskID: task.ID})
		return nil
	})
	return task, err
}
func cloneTask(t a2a.Task) a2a.Task {
	data, _ := json.Marshal(t)
	var out a2a.Task
	_ = json.Unmarshal(data, &out)
	return out
}
func (s *Service) Get(id string) (a2a.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := find(&s.db, id)
	if r == nil {
		return a2a.Task{}, ErrNotFound
	}
	return cloneTask(r.Task), nil
}
func (s *Service) Tasks() []a2a.Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]a2a.Task, 0, len(s.db.Records))
	for _, r := range s.db.Records {
		out = append(out, cloneTask(r.Task))
	}
	return out
}
func (s *Service) Wait(ctx context.Context, id string) (a2a.Task, error) {
	for {
		s.mu.Lock()
		if s.storageErr != nil {
			err := s.storageErr
			s.mu.Unlock()
			return a2a.Task{}, err
		}
		r := find(&s.db, id)
		if r == nil {
			s.mu.Unlock()
			return a2a.Task{}, ErrNotFound
		}
		t := cloneTask(r.Task)
		changed := s.changed
		s.mu.Unlock()
		if a2a.Terminal(t.Status.State) {
			return t, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return a2a.Task{}, ctx.Err()
		}
	}
}
func (s *Service) Cancel(id string) (a2a.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := find(&s.db, id)
	if r == nil {
		return a2a.Task{}, ErrNotFound
	}
	if a2a.Terminal(r.Task.Status.State) {
		return a2a.Task{}, ErrTerminal
	}
	var task a2a.Task
	err := s.change(func(db *database) error {
		r := find(db, id)
		s.finish(r, a2a.Canceled, "Cancellation requested; completed effects are not undone.")
		task = r.Task
		return nil
	})
	if err == nil {
		if cancel := s.active[id]; cancel != nil {
			cancel()
		}
	}
	return task, err
}
func (s *Service) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		if s.storageErr != nil {
			err := s.storageErr
			s.mu.Unlock()
			return err
		}
		id := ""
		prompt := ""
		depth := 0
		for _, r := range s.db.Records {
			if r.Task.Status.State == a2a.Submitted {
				id = r.Task.ID
				prompt = r.Prompt
				depth = r.DelegationDepth
				break
			}
		}
		if id == "" {
			s.mu.Unlock()
			select {
			case <-s.wake:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		expired := false
		err := s.change(func(db *database) error {
			r := find(db, id)
			if !r.ExpiresAt.IsZero() && !s.now().Before(r.ExpiresAt) {
				s.finish(r, a2a.Rejected, "Event expired while queued.")
				expired = true
			} else {
				r.Task.Status = a2a.Status{State: a2a.Working, Timestamp: s.now().UTC()}
			}
			return nil
		})
		taskCtx, cancel := context.WithTimeout(a2a.WithDelegationDepth(ctx, depth), s.config.Timeout())
		if err == nil && !expired {
			s.active[id] = cancel
		}
		s.mu.Unlock()
		if err != nil {
			cancel()
			return err
		}
		if expired {
			cancel()
			continue
		}
		text, runErr := s.runner(taskCtx, prompt)
		cancel()
		s.mu.Lock()
		delete(s.active, id)
		err = s.change(func(db *database) error {
			r := find(db, id)
			if r.Task.Status.State == a2a.Canceled {
				return nil
			}
			state := a2a.Completed
			if runErr != nil {
				state = a2a.Failed
				text = runErr.Error()
				if errors.Is(runErr, context.Canceled) {
					state = a2a.Canceled
				}
			}
			s.finish(r, state, text)
			return nil
		})
		s.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

type SensorEvent struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Source     string          `json:"source"`
	Timestamp  time.Time       `json:"timestamp"`
	Confidence *float64        `json:"confidence,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}
type EventResult struct {
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
	TaskID   string `json:"task_id,omitempty"`
}

func (s *Service) Event(event SensorEvent) (EventResult, error) {
	if event.ID == "" || len(event.ID) > 256 || event.Type == "" || len(event.Type) > 256 || event.Source == "" || len(event.Source) > 256 || event.Timestamp.IsZero() || len(event.Data) > 16000 {
		return EventResult{}, errors.New("event requires bounded id, type, source, timestamp, and data")
	}
	if len(event.Data) > 0 && !json.Valid(event.Data) {
		return EventResult{}, errors.New("event data must be valid JSON")
	}
	if event.Confidence != nil && (*event.Confidence < 0 || *event.Confidence > 1) {
		return EventResult{}, errors.New("confidence must be between 0 and 1")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := "event:" + digest([]string{event.Source, event.ID})
	hash := digest(event)
	prior, err := receiptFor(&s.db, key, hash)
	if err != nil {
		return EventResult{}, err
	}
	if prior != nil {
		return EventResult{Accepted: prior.TaskID != "", TaskID: prior.TaskID, Reason: prior.Reason}, nil
	}
	// Duplicates above do not consume the durable-ingress budget.
	if !s.eventLimiter.AllowN(s.now(), 1) {
		return EventResult{}, ErrEventRate
	}
	result := EventResult{Reason: "no matching trigger"}
	err = s.change(func(db *database) error {
		for _, rule := range s.config.Triggers {
			if rule.Type != event.Type || rule.Source != event.Source {
				continue
			}
			triggerKey := rule.Source + "\x00" + rule.Type
			state := db.Triggers[triggerKey]
			now := s.now()
			if event.Timestamp.After(now.Add(5*time.Second)) || now.Sub(event.Timestamp) > time.Duration(rule.MaxAgeSeconds)*time.Second {
				result.Reason = "stale or future event"
				break
			}
			if !state.Last.IsZero() && !event.Timestamp.After(state.Last) {
				result.Reason = "out-of-order event"
				break
			}
			if event.Timestamp.Sub(state.Last) > time.Duration(rule.MaxGapSeconds)*time.Second {
				state.Count = 0
			}
			state.Last = event.Timestamp
			if event.Confidence == nil && rule.MinConfidence > 0 || event.Confidence != nil && *event.Confidence < rule.MinConfidence {
				state.Count = 0
				db.Triggers[triggerKey] = state
				result.Reason = "confidence below threshold"
				break
			}
			state.Count++
			switch {
			case !state.Fired.IsZero() && now.Sub(state.Fired) < time.Duration(rule.CooldownSeconds)*time.Second:
				result.Reason = "cooldown"
			case state.Count < rule.Consecutive:
				result.Reason = "waiting for consecutive observations"
			default:
				task, err := s.add(db, sensorEventPrompt(rule.Prompt, event), "", event.Timestamp.Add(time.Duration(rule.MaxAgeSeconds)*time.Second))
				if err != nil {
					return err
				}
				result = EventResult{Accepted: true, TaskID: task.ID}
				state.Fired = now
				state.Count = 0
			}
			db.Triggers[triggerKey] = state
			break
		}
		remember(db, receipt{Key: key, Hash: hash, TaskID: result.TaskID, Reason: result.Reason})
		return nil
	})
	return result, err
}

func (s *Service) String() string { return fmt.Sprintf("%s (%s)", s.config.Name, s.config.Profile) }

func sensorEventPrompt(instructions string, event SensorEvent) string {
	data, _ := json.Marshal(event)
	// Encode the JSON as a string as well, so payloads cannot close the boundary.
	escaped, _ := json.Marshal(string(data))
	return instructions + "\n\n<untrusted_sensor_event_json>\n" + string(escaped) + "\n</untrusted_sensor_event_json>"
}
