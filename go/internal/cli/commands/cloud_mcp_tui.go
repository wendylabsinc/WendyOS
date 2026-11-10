package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/cancelreader"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"golang.org/x/term"
)

// promptTerminal owns stdin for the whole session. Keys entered while no
// question is displayed are discarded instead of answering the next question.
type promptTerminal struct {
	mu      sync.Mutex
	out     io.Writer
	ctx     context.Context
	cancel  context.CancelFunc
	program *tea.Program
	done    chan struct{}
	err     error
}

func newPromptTerminal(ctx context.Context, in io.Reader, out io.Writer) *promptTerminal {
	ctx, cancel := context.WithCancel(ctx)
	t := &promptTerminal{ctx: ctx, out: out, cancel: cancel, done: make(chan struct{})}
	input, err := newPromptInput(in, cancel)
	if err != nil {
		t.err = err
		cancel()
		close(t.done)
		return t
	}
	var reader io.Reader
	if input != nil {
		reader = input
	}
	t.program = tea.NewProgram(promptModel{}, tea.WithContext(ctx), tea.WithInput(reader), tea.WithOutput(out), tea.WithoutSignalHandler(), tea.WithFilter(func(model tea.Model, msg tea.Msg) tea.Msg {
		if key, ok := msg.(tea.KeyMsg); ok {
			return promptKey{key: key, at: model.(promptModel).inputAt}
		}
		return msg
	}))
	if input != nil {
		input.program = t.program
	}
	go func() {
		_, err := tui.RunInlineProgram(t.program)
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, tea.ErrInterrupted) {
			t.err = err
		}
		if input != nil {
			t.err = errors.Join(t.err, input.close())
		}
		cancel()
		close(t.done)
	}()
	return t
}

func (t *promptTerminal) close() error {
	if t.program != nil {
		t.program.Quit()
	}
	<-t.done
	return t.err
}

func (t *promptTerminal) printf(format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ctx.Err() != nil {
		<-t.done // Restore the terminal before printing the final session message.
		fmt.Fprintf(t.out, format, args...)
		return
	}
	t.program.Println(strings.TrimSuffix(fmt.Sprintf(format, args...), "\n"))
}

func (t *promptTerminal) ask(ctx context.Context, question string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ctx.Err() != nil || t.ctx.Err() != nil {
		return false
	}
	reply := make(chan bool, 1)
	t.program.Send(promptRequest{ctx: ctx, question: question, reply: reply, at: time.Now()})
	select {
	case answer := <-reply:
		return answer && ctx.Err() == nil && t.ctx.Err() == nil
	case <-ctx.Done():
		t.program.Send(promptDismiss{reply: reply})
		return false
	case <-t.ctx.Done():
		return false
	}
}

type promptRequest struct {
	ctx      context.Context
	question string
	reply    chan bool
	at       time.Time
}

type promptDismiss struct{ reply chan bool }

type promptInputRead struct{ at time.Time }

type promptKey struct {
	key tea.KeyMsg
	at  time.Time
}

type promptModel struct {
	request *promptRequest
	confirm tui.ConfirmModel
	inputAt time.Time
}

func (m promptModel) Init() tea.Cmd { return nil }

func (m promptModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case promptInputRead:
		m.inputAt = msg.at
	case promptRequest:
		if msg.ctx.Err() != nil {
			msg.reply <- false
			return m, nil
		}
		msg.at = time.Now()
		m.request = &msg
		m.confirm = tui.NewConfirm(msg.question)
	case promptDismiss:
		if m.request != nil && m.request.reply == msg.reply {
			m.request = nil
		}
	case promptKey:
		if msg.key.Type == tea.KeyCtrlC {
			m.request = nil
			return m, tea.Quit
		}
		if m.request == nil || msg.at.Before(m.request.at) || msg.key.Paste {
			return m, nil
		}
		if m.request.ctx.Err() != nil {
			m.request.reply <- false
			m.request = nil
			return m, nil
		}
		next, cmd := m.confirm.Update(msg.key)
		m.confirm = next.(tui.ConfirmModel)
		if m.confirm.Cancelled() {
			m.request = nil
			return m, tea.Quit
		}
		if cmd != nil {
			m.request.reply <- m.confirm.Confirmed()
			m.request = nil
		}
	}
	return m, nil
}

func (m promptModel) View() string {
	if m.request == nil {
		return ""
	}
	return m.confirm.View()
}

// promptInput stamps each complete read before Bubble Tea decodes its keys.
// The marker and the following keys use the same ordered message channel, so
// even the unused keys from a batch cannot answer a later question.
//
// This deliberately does not expose Fd: on Windows Bubble Tea opens its own
// console reader for file inputs, which would bypass these timestamps. We own
// raw mode and cancellation here instead, on every platform.
type promptInput struct {
	reader  cancelreader.CancelReader
	program *tea.Program
	cancel  context.CancelFunc
	restore func() error
	mu      sync.Mutex
	closed  bool
	reads   sync.WaitGroup
}

func newPromptInput(in io.Reader, cancel context.CancelFunc) (*promptInput, error) {
	if in == nil {
		return nil, nil
	}
	p := &promptInput{cancel: cancel}
	if file, ok := in.(interface{ Fd() uintptr }); ok && term.IsTerminal(int(file.Fd())) {
		fd := int(file.Fd())
		state, err := term.MakeRaw(fd)
		if err != nil {
			return nil, fmt.Errorf("prepare prompt terminal: %w", err)
		}
		p.restore = func() error { return term.Restore(fd, state) }
	}
	reader, err := cancelreader.NewReader(in)
	if err != nil {
		if p.restore != nil {
			err = errors.Join(err, p.restore())
		}
		return nil, fmt.Errorf("prepare prompt input: %w", err)
	}
	p.reader = reader
	return p, nil
}

func (p *promptInput) Read(buf []byte) (int, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, io.EOF
	}
	p.reads.Add(1)
	p.mu.Unlock()
	defer p.reads.Done()
	n, err := p.reader.Read(buf)
	if errors.Is(err, io.EOF) {
		p.cancel()
	}
	if n > 0 {
		p.program.Send(promptInputRead{at: time.Now()})
	}
	return n, err
}

func (p *promptInput) close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	if p.reader.Cancel() {
		p.reads.Wait()
	}
	err := p.reader.Close()
	if p.restore != nil {
		err = errors.Join(err, p.restore())
	}
	return err
}
